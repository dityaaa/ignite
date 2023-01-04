package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/dityaaa/ignite/config"
	"github.com/dityaaa/ignite/runner"
	"github.com/dityaaa/ignite/version"
)

var (
	flagInitConfig = flag.Bool("init", false, "Create a default configuration file in the current directory.")
	flagConfigPath = flag.String("config", "./"+config.DefaultConfigFileName, "Path to the configuration file.")
	flagAppVersion = flag.Bool("version", false, "Show ignite version.")
	flagVerbose    = flag.Bool("verbose", false, "Verbose logging.")
	flagBuildTags  = flag.String("tags", "", "Anything provided to 'go run' or 'go build' -tags.")
)

func main() {
	flag.Parse()

	// Instantly exit when user only want to see current app version
	if *flagAppVersion {
		fmt.Println(version.V)
		os.Exit(0)
		return
	}

	// Instantly exit when user tries to initialize new configuration file
	if *flagInitConfig {
		err := config.CreateDefaultConfig()
		if err != nil {
			log.Fatalln("Could not create default config file.", err)
			return
		}

		os.Exit(0)
		return
	}

	err := config.Read(*flagConfigPath, false)
	if err != nil {
		log.Fatalln("Could not parse config file.", errors.Unwrap(err))
		return
	}

	//Handle overriding config with flags.
	if len(strings.TrimSpace(*flagBuildTags)) > 0 {
		if !config.Data().UsingDefaults() {
			log.Println("WARNING! (main) Overriding Tags with provided -tags.")
		}
		config.Data().OverrideTags(*flagBuildTags)
	}
	if *flagVerbose {
		config.Data().OverrideVerbose(*flagVerbose)
	}

	//Configure.
	err = runner.Configure()
	if err != nil {
		log.Fatal("Error with configure", err)
		return
	}

	//Watch for changes to files.
	runner.Watch()

	//Run.
	runner.Start()
}
