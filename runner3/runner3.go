/*
Package runner3 handles building and running the binary when a file has been modified.

How watching/building/running works:
  - The directory tree starting at the root working directory is walked (recursively).
  - If the directory is not ignored, per the config file settings, an fsnotify watcher
    is set on each directory to watch for changes to files within that directory.
  - When a file change event occurs, the file is checked to see if it has a watched
    extension. If yes, the binary is rebuilt and/or rerun as needed.
*/
package runner3

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/c9845/fresher/config"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/exp/slices"
)

// Define log for anything fresher related. This logging output is controlled by
// fresher, prefixed, and colored so it stands out from logging output from the
// binary being build/run.
var fresherLog *coloredLogger

// Define communication channels.
var (
	//eventsChan relays file change events from the file change watcher to the binary
	//builder. Messages are in the format `"file: DELETE|MODIFY|...`.
	//See: https://pkg.go.dev/github.com/fsnotify/fsnotify#Event.String
	eventsChan = make(chan string, 1000)

	//stopChan is for terminating the built and running binary when the binary is
	//rebuilt. This prevents multiple copies of the binary from running concurrently.
	stopChan = make(chan bool)

	//killBuildingChan is used to signal to build() that the `go build...` command should
	//be terminated. This is used when another file change event has occured while
	//build() is running that will just cause build() to run again. There is no sense
	//in completing the currently running build since build() will just be called
	//again immediately after completing. Killing off the running build just saves a
	//bit of time.
	killBuildingChan = make(chan bool, 1)
)

// Configure handles some initialization steps before watching for file changes and
// handling building and running the binary.
func Configure() (err error) {
	//Set up the logging.
	fresherLog = newLogger("fresher", "blue")

	//Set the number of maximum file descriptors that can be opened by this process.
	//This is needed for watching a HUGE amount of files. Windows is not applicable.
	if runtime.GOOS != "windows" {
		var rLimit syscall.Rlimit
		rLimit.Max = 10000
		rLimit.Cur = 10000
		err = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit)
		if err != nil {
			fresherLog.Println("Configure", "Error setting rlimit.", err)
			return
		}
	}

	//Create the temp directory to store the build binary and error logs.
	err = os.MkdirAll(config.Data().TempDir, 0755)
	if err != nil {
		return
	}

	//TODO: write file to temp directory explaining what it is for?
	//TODO: add tmp directory to .gitignore?

	return
}

// Watch handles setting up the watcher of file changes. The watcher is populated with
// a list of directories to watch, not individual files. Some directories are ignored
// per the config file field DirectoriesToIgnore.
//
// When a file change event occurs, the event is sent on the eventsChan which will be
// recevied in start() and is used to trigger the binary being built via build().
func Watch() (err error) {
	//Initialize the watcher.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	//Add paths to watcher of the directories to watch for file changes. We watch
	//directories, not individual files, for changes.
	//
	//This works by walking the directory starting at the working directory, typically
	//the directory fresher is being run in, checking if each directory should be
	//watched or ignored (as set in config file), and adding the directory to the
	//watcher.
	err = filepath.WalkDir(config.Data().WorkingDir, func(path string, d fs.DirEntry, err error) error {
		//Handle errors related to the path. See fs.WalkDirFunc for more info.
		if err != nil {
			return err
		}

		//Only watch directories, not individual files.
		if !d.IsDir() {
			return nil
		}

		//Ignore directory if it is the temp directory where built binaries are stored
		//before running. No need to watch this directory since it stores temp data
		//from fresher.
		yes, err := config.IsTempDir(path)
		if err != nil {
			return err
		}
		if yes {
			return fs.SkipDir
		}

		//Ignore directory if it is in list of ignored directories. Ignored directories
		//listed in config file are based off of the WorkingDir. The path in the
		//WalkDirFunc here is also based off of the WorkingDir, so therefore we can
		//easily compare without having to handle absolute paths.
		if config.IsDirectoryToIgnore(path) {
			if config.Data().VerboseLogging {
				fresherLog.Println("IGNORING...", path)
			}

			return fs.SkipDir
		}

		//Add path to watcher.
		if config.Data().VerboseLogging {
			fresherLog.Println("Watching...", path)
		}
		err = watcher.Add(path)
		return err
	})
	if err != nil && err != fs.SkipDir {
		return
	}

	//Watch for file change events. When an event does occur, make sure it is a
	//file write (not CHMOD or something else) and that the file that was changed has
	//an extension that we watch for (i.e.: no sense in sending events to rebuild
	//binary if a .docx file was changed).
	go func() {
		for {
			select {
			case err := <-watcher.Errors:
				if err != nil {
					fresherLog.Println("watcher error", err)
				}

			case event := <-watcher.Events:
				//Ignore event on certain events.
				if event.Op == fsnotify.Chmod {
					continue
				}

				//Get name of file that has changed (event.Name is actually a relative
				//path, not just a file's name).
				eventName := event.Name
				eventType := event.Op.String()

				//Skip building if a non-watched file is changed.
				if !config.HasExtensionToWatch(eventName) {
					continue
				}

				if config.Data().VerboseLogging {
					fresherLog.Println("SENDING EVENT...", eventType, eventName)
				}

				eventsChan <- eventName

				//Check if binary is currently being built and stop the build if this
				//event will just result in a rebuild. This saves a bit of time since
				//we don't build the binary twice (once is ongoing and again for the
				//new event) and have to wait for the first build to complete before
				//the second build starts.
				//
				//This is not checked in start() since start blocks when build() is
				//running and thus will not be able to receive a new event until build()
				//is complete, therefore building can never be killed!
				if buildCmdRunning && shouldRebuild(eventName) {
					killBuildingChan <- true
				}
			}
		}
	}()

	//Watcher is set up to watch for changes in directories.
	//goroutine watching for file change events will continue running.

	return
}

// start watches for file change events and runs the commands to build and run the
// binary.
func start() {
	//Is fresher running the binary. If yes, and a build error occurs, the currently
	//running binary won't be stopped. If no, then the binary isn't running and this
	//is most likely the first time fresher has been run, therefore just exit fresher
	//on a build error.
	started := false

	//Wait for file change events to rebuild and rerun the binary. This waits for
	//file change events sent on the eventsChan as set up in Watch().
	go func() {
		for {
			//Get event.
			eventName := <-eventsChan
			fresherLog.Println("GOT EVENT...", eventName)

			//Track if build is successful so we know to stop watching and building.
			buildFailed := false
			buildKilled := false

			//Determine if we need to rebuild the binary. We really only need to
			//rebuild if a .go file changes (unless the binary is using embedded
			//files). This is simply a performance improver since we do not need to
			//rebuild the binary if, say, an HTML file is changed.
			if shouldRebuild(eventName) {
				//Binary should be rebuilt.

				//Get build delay so that we don't rebuild too fast. This helps improve
				//performance a bit when multiple file events occur in rapid succession
				//since the binary won't be built, the build cancelled (see
				//killBuildingChan channel), then the build starting again, etc.
				//
				//The build delay should be low enough not to induce too much latency
				//before building but long enough to catch rapid file saves.
				time.Sleep(time.Duration(config.Data().BuildDelayMilliseconds) * time.Millisecond)

				//Clear the error log since we are rebuilding the binary.
				err := deleteBuildErrorsLog()
				if err != nil && !os.IsNotExist(err) {
					fresherLog.Println("Error deleting build log.", err)
					//not exiting on error since this isn't an end-of-the-world event.
				}

				//Build the binary. Same as running `go build`.
				err = build()
				if err == errBuildKilled {
					buildKilled = true

					if config.Data().VerboseLogging {
						fresherLog.Println("BUILD KILLED, BUILD WILL BE RERUN...")
					}
				} else if err != nil {
					buildFailed = true

					fresherLog.Println("Build Failed", err)
					if !started {
						//Build failed and the binary never stared running, exit fresher.
						//This should only occur when fresher just starts and builds
						//the binary for the first time.
						os.Exit(1)
					}
				}
			}

			//Run the built binary if it was successfully built. Same as running
			//./my-binary or .\my-binary.exe. Binary is also rerun if a file change
			//event occured that didn't required a rebuild.
			//
			//Stop the binary if it was previously built and is running before
			//restarting it. We don't want more than one build running at once!
			if !buildFailed && !buildKilled {
				if started {
					stopChan <- true
				}

				run()

				//Add logging line to separate fresher logging output from built
				//binary's logging output.
				fresherLog.Println(strings.Repeat("-", 50))
			}

			//Note that binary is started. This way if a subsequent build fails, the
			//running binary won't be stopped.
			started = true
		}
	}()
}

// deleteBuildErrorsLog deletes the build errors log file located at the path noted in
// the config file field BuildLogFilename within the TempDir. Each time a new binary
// is built the error log is deleted and recreated if another error occurs.
func deleteBuildErrorsLog() (err error) {
	pathToFile := filepath.Join(config.Data().TempDir, config.Data().BuildLogFilename)
	err = os.Remove(pathToFile)
	return
}

// shouldRebuild checks if the file noted in eventName ends in a file extension that
// we watch for changes and rebuild the binary on. This compares the extension to the
// config file field NoRebuildExtensions. eventName is a relative path, not just a
// file's name.
//
// This func is not located in the config package since it handles fsnotify specific
// data/text and all fsnotify related code is in this package.
func shouldRebuild(eventName string) bool {
	//Get file's extension. This includes the prepended period (i.e: .html will be
	//returned by Ext().
	extension := filepath.Ext(eventName)

	//Check if filename has an extension that rebuilding should be skipped.
	return !slices.Contains(config.Data().NoRebuildExtensions, extension)
}

// buildCmdRunning is used to monitor the state of whether or not the `go build`
// command is running. This is set in build() when .Start() is called and reset when
// after .Wait() stops blocking.
//
// This is read in start() to check if the binary is currently being built when a new
// file change event occurs and determines if a message is sent on the killBuildingChan.
var buildCmdRunning bool = false

// errBuildFailed is returned by build() when a build fails.
var errBuildFailed = errors.New("build failed")

// errBuildKilled is returned by build() when a build is killed in the middle of
// building due to a message on the killBuildingChan channel. This isn't really an
// error since the binary will just be rebuilt (similar error in usage as fs.SkipDir).
var errBuildKilled = errors.New("build killed")

// build builds the binary. This runs `go build` and outputs a binary to the temp
// directory noted in the config file.
//
// A string is returned only upon an stderr output when an stderr occurs in `go build`.
// True is returned when build is successful.
//
// build() is called in start().
func build() (err error) {
	//Get path and name to output built binary as. This is a file located in the
	//temp directory.
	pathToBuiltBinary := getPathToBuiltBinary()

	//Get path to entry point of app. This is typically just the repository root.
	//Not naming a single "main.go" file allows for using any .go filename as the
	//entry point, and using multiple .go files at once; go build figures this all
	//out.
	entryPoint := config.Data().WorkingDir

	//Build arguments passed to "go" command.
	args := []string{
		"build",
		"-o", pathToBuiltBinary,
	}

	//Handle build tags.
	if len(config.Data().GoBuildTags) > 0 {
		tags := strings.Join(config.Data().GoBuildTags, " ")
		args = append(args, "-tags", tags)
	}
	args = append(args, entryPoint)

	//TODO: handle other go build flags.

	//Initialize the command, but do not run it.
	cmd := exec.Command("go", args...)
	if config.Data().VerboseLogging {
		fresherLog.Println("Building...", "go", strings.Join(args, " "))
	} else {
		fresherLog.Println("Building...")
	}

	//Set up logging for when the command runs. We want to capture the output logging
	//and output it to the user running fresher. This is so the user can see any output
	//from running `go build` to diagnose issues.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}

	//Start handler to kill builds if needed. This is used to stop builds when another
	//file change will cause build() to be run again. Since we are just going to
	//build the binary again almost instantly after this build completes, we can kill
	//off the running build.
	//
	//We use cancelKiller to stop the goroutine once a build completes successfully,
	//this way we don't have this goroutine sitting around needlessly after a build
	//is completed.
	//
	//buildKilled is used to return a specific error when a build is killed.
	cancelKiller := make(chan bool, 1)
	buildKilled := false
	go func() {
		select {
		case x := <-killBuildingChan:
			if x {
				fresherLog.Println("Building...killed")

				err := cmd.Process.Kill()
				if err != nil {
					fresherLog.Println("Killing build error", err)
				}

				buildKilled = true
			}

		case <-cancelKiller:
			//Terminate this goroutine since build was completed. This was we don't
			//end up with endlessly running goroutines.
		}
	}()

	//Run the command, go build...
	buildCmdRunning = true
	err = cmd.Start()
	if err != nil {
		return
	}

	//Copy output for stdout to fresher's stdout. This way user sees output from
	//building.
	_, err = io.Copy(os.Stdout, stdout)
	if err != nil {
		return
	}

	//Capture stderr since it might have a bunch of diagnostic info about why built
	//failed. Stderr is saved to error file so that it is easier to inspect then
	//reading in terminal output.
	errBuf, err := io.ReadAll(stderr)
	if err != nil {
		fresherLog.Println("Error capturing stderr", err)
	}

	//Wait for command to finish. Have to handle build being killed by us!
	err = cmd.Wait()
	if err != nil && buildKilled {
		return errBuildKilled
	} else if err != nil {
		return
	}

	//Build is complete. Stop the goroutine the monitors if the build should be stopped
	//while it is running (in case another file change event occured while building
	//that would just cause the binary to be built again anyway).
	buildCmdRunning = false
	cancelKiller <- true

	//If an error occured, write the output to a log file. There could be useful info
	//such as stack traces or other logging to identify issue in this error.
	if len(errBuf) > 0 {
		saveBuildErrorsLog(string(errBuf))
		return errBuildFailed
	}

	//Build was successful. Binary is now located in temp dir.
	return
}

// getPathToBuiltBinary returns the path to where the build binary will be saved.
// Basically, append BuildName to TempDir and add .exe if needed.
func getPathToBuiltBinary() string {
	path := filepath.Join(config.Data().TempDir, config.Data().BuildName)
	if runtime.GOOS == "windows" && filepath.Ext(path) != ".exe" {
		path += ".exe"
	}

	return path
}

// saveBuildErrorsLog saves the stderr output from `go build` when build() is called
// to a file. This file is deleted each time a build is attempted via
// deleteBuildErrorsLog which is called in start().
func saveBuildErrorsLog(message string) {
	//Get path to log file.
	pathToFile := filepath.Join(config.Data().TempDir, config.Data().BuildLogFilename)

	//Create the file.
	f, err := os.Create(pathToFile)
	if err != nil {
		fresherLog.Println("Could not create log file", err)
		//not exiting on error since we don't do anything with error anyway.
	}
	defer f.Close()

	//Write to file
	_, err = f.WriteString(message)
	if err != nil {
		fresherLog.Println("Could not write log file", err)
		//not exiting on error since we don't do anything with error anyway.
	}
}

// run runs the binary build in build().
//
// run() is called in start().
func run() {
	//Get path to built binary.
	pathToBuiltBinary := getPathToBuiltBinary()

	//Initialize the command, but do not run it.
	cmd := exec.Command(pathToBuiltBinary)
	if config.Data().VerboseLogging {
		fresherLog.Println("Running...", pathToBuiltBinary)
	} else {
		fresherLog.Println("Running...")
	}

	//Set up logging for when the command runs. We want to capture the output logging
	//and output it to the user running fresher. This is so the user can see any output
	//from running the binary to diagnose issues.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		fresherLog.Fatalln(err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fresherLog.Fatalln(err)
	}

	//Run the command/binary.
	err = cmd.Start()
	if err != nil {
		fresherLog.Fatalln(err)
	}

	//Copy output from the command to output from fresher. This way the output from
	//the binary is displayed to the user in real time.
	go io.Copy(os.Stderr, stderr)
	go io.Copy(os.Stdout, stdout)

	//Stop the running binary if it has been rebuilt and will be rerun. This prevents
	//multiple built binaries from running at one time.
	go func() {
		<-stopChan
		cmd.Process.Kill()
	}()
}

// Start calls start() to handle building the running the binary.
func Start() {
	start()

	//Send an event to build and run the binary for the first time when fresher
	//starts. "/" is just a random string to trigger building.
	eventsChan <- "/"

	//Block indefintely to continuously watch for file changes and rebuild as needed.
	<-make(chan int)
}