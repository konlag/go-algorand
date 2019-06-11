// Copyright (C) 2019 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/daemon/algod/api/client"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/logging/telemetryspec"
	"github.com/algorand/go-algorand/nodecontrol"
	"github.com/algorand/go-algorand/shared/algoh"
	"github.com/algorand/go-algorand/util"
)

var dataDirectory = flag.String("d", "", "Root Algorand daemon data path")
var versionCheck = flag.Bool("v", false, "Display and write current build version and exit")
var telemetryOverride = flag.String("t", "", `Override telemetry setting if supported (Use "true", "false", "0" or "1")`)

const algodFileName = "algod"
const goalFileName = "goal"

var exeDir string

func init() {
}

type stdCollector struct {
	output string
}

func (c *stdCollector) Write(p []byte) (n int, err error) {
	s := string(p)
	c.output += s
	return len(p), nil
}

func main() {
	flag.Parse()
	nc := getNodeController()

	genesisID, err := nc.GetGenesisID()
	if err != nil {
		fmt.Fprintln(os.Stdout, "error loading telemetry config", err)
		return
	}

	dataDir := ensureDataDir()
	absolutePath, absPathErr := filepath.Abs(dataDir)
	config.UpdateVersionDataDir(absolutePath)

	if *versionCheck {
		version := config.GetCurrentVersion()
		versionInfo := version.AsUInt64()
		fmt.Printf("%d\n%s.%s [%s] (commit #%s)\n", versionInfo, version.String(),
			version.Channel, version.Branch, version.GetCommitHash())
		return
	}

	// If data directory doesn't exist, we can't run. Don't bother trying.
	if len(dataDir) == 0 {
		fmt.Fprintln(os.Stderr, "Data directory not specified.  Please use -d or set $ALGORAND_DATA in your environment.")
		os.Exit(1)
	}

	if absPathErr != nil {
		reportErrorf("Can't convert data directory's path to absolute, %v\n", dataDir)
	}

	if _, err := os.Stat(absolutePath); err != nil {
		reportErrorf("Data directory %s does not appear to be valid\n", dataDir)
	}

	config, err := algoh.LoadConfigFromFile(filepath.Join(dataDir, algoh.ConfigFilename))
	if err != nil && !os.IsNotExist(err) {
		reportErrorf("Error loading configuration, %v\n", err)
	}
	validateConfig(config)

	log := logging.Base()
	configureLogging(genesisID, log, absolutePath)
	defer log.CloseTelemetry()

	exeDir, err = util.ExeDir()
	if err != nil {
		reportErrorf("Error getting ExeDir: %v\n", err)
	}

	var errorOutput stdCollector
	var output stdCollector
	done := make(chan struct{})
	go func() {
		args := make([]string, len(os.Args)-1)
		copy(args, os.Args[1:]) // Copy our arguments (skip the executable)
		if log.GetTelemetryEnabled() {
			args = append(args, "-s", log.GetTelemetrySession())
		}
		algodPath := filepath.Join(exeDir, algodFileName)
		cmd := exec.Command(algodPath, args...)
		cmd.Stderr = &errorOutput
		cmd.Stdout = &output

		err = cmd.Start()
		if err != nil {
			reportErrorf("Error starting algod: %v", err)
		}
		cmd.Wait()
		close(done)

		log.Infoln("++++++++++++++++++++++++++++++++++++++++")
		log.Infoln("algod exited.  Exiting...")
		log.Infoln("++++++++++++++++++++++++++++++++++++++++")
	}()

	// Set up error capturing in case algod exits before we can get REST client
	defer func() {
		if errorOutput.output != "" {
			fmt.Fprintf(os.Stderr, errorOutput.output)
			details := telemetryspec.ErrorOutputEventDetails{
				Error:  errorOutput.output,
				Output: output.output,
			}
			log.EventWithDetails(telemetryspec.HostApplicationState, telemetryspec.ErrorOutputEvent, details)

			// Write stdout & stderr streams to disk
			ioutil.WriteFile(filepath.Join(absolutePath, nodecontrol.StdOutFilename), []byte(output.output), os.ModePerm)
			ioutil.WriteFile(filepath.Join(absolutePath, nodecontrol.StdErrFilename), []byte(errorOutput.output), os.ModePerm)

			if config.UploadOnError {
				fmt.Fprintf(os.Stdout, "Uploading logs...\n")
				sendLogs()
			}
		}
	}()

	// Handle signals cleanly
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	signal.Ignore(syscall.SIGHUP)
	go func() {
		sig := <-c
		fmt.Printf("Exiting algoh on %v\n", sig)
		os.Exit(0)
	}()

	client, err := waitForClient(nc, done)
	if err != nil {
		reportErrorf("error creating Rest Client: %v\n", err)
	}

	var wg sync.WaitGroup

	deadMan := makeDeadManWatcher(config.DeadManTimeSec, client, config.UploadOnError, done, &wg)
	wg.Add(1)

	listeners := []blockListener{deadMan}
	if config.SendBlockStats {
		// Note: Resume can be implemented here. Store blockListener state and set curBlock based on latestBlock/lastBlock.
		listeners = append(listeners, &blockstats{log: logging.Base()})
	}

	delayBetweenStatusChecks := time.Duration(config.StatusDelayMS) * time.Millisecond
	stallDetectionDelay := time.Duration(config.StallDelayMS) * time.Millisecond

	runBlockWatcher(listeners, client, done, &wg, delayBetweenStatusChecks, stallDetectionDelay)
	wg.Add(1)

	wg.Wait()
	fmt.Println("Exiting algoh normally...")
}

func waitForClient(nc nodecontrol.NodeController, abort chan struct{}) (client client.RestClient, err error) {
	for {
		client, err = getRestClient(nc)
		if err == nil {
			return client, nil
		}

		select {
		case <-abort:
			err = fmt.Errorf("Aborted waiting for client")
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func getRestClient(nc nodecontrol.NodeController) (rc client.RestClient, err error) {
	// Fetch the algod client
	algodClient, err := nc.AlgodClient()
	if err != nil {
		return
	}

	// Make sure the node is running
	_, err = algodClient.Status()
	if err != nil {
		return
	}

	return algodClient, nil
}

func resolveDataDir() string {
	// Figure out what data directory to tell algod to use.
	// If not specified on cmdline with '-d', look for default in environment.
	var dir string
	if dataDirectory == nil || *dataDirectory == "" {
		dir = os.Getenv("ALGORAND_DATA")
	} else {
		dir = *dataDirectory
	}
	return dir
}

func ensureDataDir() string {
	// Get the target data directory to work against,
	// then handle the scenario where no data directory is provided.
	dir := resolveDataDir()
	if dir == "" {
		reportErrorf("Data directory not specified.  Please use -d or set $ALGORAND_DATA in your environment. Exiting.\n")
	}
	return dir
}

func getNodeController() nodecontrol.NodeController {
	binDir, err := util.ExeDir()
	if err != nil {
		panic(err)
	}
	nc := nodecontrol.MakeNodeController(binDir, ensureDataDir())
	return nc
}

func configureLogging(genesisID string, log logging.Logger, rootPath string) {
	log = logging.Base()

	liveLog := fmt.Sprintf("%s/host.log", rootPath)
	fmt.Println("Logging to: ", liveLog)
	writer, err := os.OpenFile(liveLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		panic(fmt.Sprintf("configureLogging: cannot open log file %v", err))
	}
	log.SetOutput(writer)
	log.SetJSONFormatter()
	log.SetLevel(logging.Debug)

	initTelemetry(genesisID, log, rootPath)

	// if we have the telemetry enabled, we want to use it's sessionid as part of the
	// collected metrics decorations.
	fmt.Fprintln(writer, "++++++++++++++++++++++++++++++++++++++++")
	fmt.Fprintln(writer, "Logging Starting")
	fmt.Fprintln(writer, "++++++++++++++++++++++++++++++++++++++++")
}

func initTelemetry(genesisID string, log logging.Logger, dataDirectory string) {
	// Enable telemetry hook in daemon to send logs to cloud
	// If ALGOTEST env variable is set, telemetry is disabled - allows disabling telemetry for tests
	isTest := os.Getenv("ALGOTEST") != ""
	if !isTest {
		telemetryConfig, err := logging.EnsureTelemetryConfig(nil, genesisID)
		if err != nil {
			fmt.Fprintln(os.Stdout, "error loading telemetry config", err)
			return
		}

		// Apply telemetry override.
		hasOverride, override := logging.TelemetryOverride(*telemetryOverride)
		if hasOverride {
			telemetryConfig.Enable = override
		}

		if telemetryConfig.Enable {
			err = log.EnableTelemetry(telemetryConfig)
			if err != nil {
				fmt.Fprintln(os.Stdout, "error creating telemetry hook", err)
				return
			}
			// For privacy concerns, we don't want to provide the full data directory to telemetry.
			// But to be useful where multiple nodes are installed for convenience, we should be
			// able to discriminate between instances with the last letter of the path.
			if dataDirectory != "" {
				dataDirectory = dataDirectory[len(dataDirectory)-1:]
			}
			if log.GetTelemetryEnabled() {
				currentVersion := config.GetCurrentVersion()
				startupDetails := telemetryspec.StartupEventDetails{
					Version:    currentVersion.String(),
					CommitHash: currentVersion.CommitHash,
					Branch:     currentVersion.Branch,
					Channel:    currentVersion.Channel,
					Instance:   dataDirectory,
				}

				log.EventWithDetails(telemetryspec.HostApplicationState, telemetryspec.StartupEvent, startupDetails)
			}
		}
	}
}

func reportErrorf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format, args...)
	logging.Base().Fatalf(format, args...)
}

func sendLogs() {
	var args []string
	args = append(args, "-d", ensureDataDir())
	args = append(args, "logging", "send")

	goalPath := filepath.Join(exeDir, goalFileName)
	cmd := exec.Command(goalPath, args...)

	err := cmd.Run()
	if err != nil {
		reportErrorf("Error sending logs: %v\n", err)
	}
}

func validateConfig(config algoh.HostConfig) {
	// Enforce a reasonable deadman timeout
	if config.DeadManTimeSec > 0 && config.DeadManTimeSec < 30 {
		reportErrorf("Config.DeadManTimeSec should be >= 30 seconds (set to %v)\n", config.DeadManTimeSec)
	}
}