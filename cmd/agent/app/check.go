// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-2019 Datadog, Inc.

package app

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/DataDog/datadog-agent/cmd/agent/common"
	"github.com/DataDog/datadog-agent/pkg/aggregator"
	"github.com/DataDog/datadog-agent/pkg/autodiscovery"
	"github.com/DataDog/datadog-agent/pkg/collector"
	"github.com/DataDog/datadog-agent/pkg/collector/check"
	"github.com/DataDog/datadog-agent/pkg/config"
	"github.com/DataDog/datadog-agent/pkg/serializer"
	"github.com/DataDog/datadog-agent/pkg/status"
	"github.com/DataDog/datadog-agent/pkg/util"
)

var (
	checkRate  bool
	checkTimes int
	checkPause int
	checkName  string
	checkDelay int
	logLevel   string
	formatJSON bool
)

// Make the check cmd aggregator never flush by setting a very high interval
const checkCmdFlushInterval = time.Hour

func init() {
	AgentCmd.AddCommand(checkCmd)

	checkCmd.Flags().BoolVarP(&checkRate, "check-rate", "r", false, "check rates by running the check twice with a 1sec-pause between the 2 runs")
	checkCmd.Flags().IntVarP(&checkTimes, "check-times", "t", 1, "number of times to run the check")
	checkCmd.Flags().IntVar(&checkPause, "pause", 0, "pause between multiple runs of the check, in milliseconds")
	checkCmd.Flags().StringVarP(&logLevel, "log-level", "l", "", "set the log level (default 'off')")
	checkCmd.Flags().IntVarP(&checkDelay, "delay", "d", 100, "delay between running the check and grabbing the metrics in miliseconds")
	checkCmd.Flags().BoolVarP(&formatJSON, "json", "", false, "format aggregator output as json")
	checkCmd.SetArgs([]string{"checkName"})
}

var checkCmd = &cobra.Command{
	Use:   "check <check_name>",
	Short: "Run the specified check",
	Long:  `Use this to run a specific check with a specific rate`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Global Agent configuration
		err := common.SetupConfig(confFilePath)
		if err != nil {
			fmt.Printf("Cannot setup config, exiting: %v\n", err)
			return err
		}

		if flagNoColor {
			color.NoColor = true
		}

		if logLevel == "" {
			if confFilePath != "" {
				logLevel = config.Datadog.GetString("log_level")
			} else {
				logLevel = "off"
			}
		} else {
			// Python calls config.Datadog.GetString("log_level")
			config.Datadog.Set("log_level", logLevel)
		}

		// Setup logger
		err = config.SetupLogger(logLevel, "", "", false, true, false)
		if err != nil {
			fmt.Printf("Cannot setup logger, exiting: %v\n", err)
			return err
		}

		if len(args) != 0 {
			checkName = args[0]
		} else {
			cmd.Help()
			return nil
		}

		hostname, err := util.GetHostname()
		if err != nil {
			fmt.Printf("Cannot get hostname, exiting: %v\n", err)
			return err
		}

		s := serializer.NewSerializer(common.Forwarder)
		agg := aggregator.InitAggregatorWithFlushInterval(s, hostname, "agent", checkCmdFlushInterval)
		common.SetupAutoConfig(config.Datadog.GetString("confd_path"))
		cs := collector.GetChecksByNameForConfigs(checkName, common.AC.GetAllConfigs())
		if len(cs) == 0 {
			for check, error := range autodiscovery.GetConfigErrors() {
				if checkName == check {
					fmt.Fprintln(color.Output, fmt.Sprintf("\n%s: invalid config for %s: %s", color.RedString("Error"), color.YellowString(check), error))
				}
			}
			for check, errors := range collector.GetLoaderErrors() {
				if checkName == check {
					fmt.Fprintln(color.Output, fmt.Sprintf("\n%s: could not load %s:", color.RedString("Error"), color.YellowString(checkName)))
					for loader, error := range errors {
						fmt.Fprintln(color.Output, fmt.Sprintf("* %s: %s", color.YellowString(loader), error))
					}
				}
			}
			for check, warnings := range autodiscovery.GetResolveWarnings() {
				if checkName == check {
					fmt.Fprintln(color.Output, fmt.Sprintf("\n%s: could not resolve %s config:", color.YellowString("Warning"), color.YellowString(check)))
					for _, warning := range warnings {
						fmt.Fprintln(color.Output, fmt.Sprintf("* %s", warning))
					}
				}
			}
			return fmt.Errorf("no valid check found")
		}

		if len(cs) > 1 {
			fmt.Println("Multiple check instances found, running each of them")
		}

		for _, c := range cs {
			s := runCheck(c, agg)

			// Sleep for a while to allow the aggregator to finish ingesting all the metrics/events/sc
			time.Sleep(time.Duration(checkDelay) * time.Millisecond)

			printMetrics(agg)

			checkStatus, _ := status.GetCheckStatus(c, s)
			fmt.Println(string(checkStatus))
		}

		if checkRate == false && checkTimes < 2 && !formatJSON {
			color.Yellow("Check has run only once, if some metrics are missing you can try again with --check-rate to see any other metric if available.")
		}

		return nil
	},
}

func runCheck(c check.Check, agg *aggregator.BufferedAggregator) *check.Stats {
	s := check.NewStats(c)
	times := checkTimes
	pause := checkPause
	if checkRate {
		if checkTimes > 2 {
			color.Yellow("The check-rate option is overriding check-times to 2")
		}
		if pause > 0 {
			color.Yellow("The check-rate option is overriding pause to 1000ms")
		}
		times = 2
		pause = 1000
	}
	for i := 0; i < times; i++ {
		t0 := time.Now()
		err := c.Run()
		warnings := c.GetWarnings()
		mStats, _ := c.GetMetricStats()
		s.Add(time.Since(t0), err, warnings, mStats)
		if pause > 0 && i < times-1 {
			time.Sleep(time.Duration(pause) * time.Millisecond)
		}
	}

	return s
}

func printMetrics(agg *aggregator.BufferedAggregator) {
	aggJSON := make(map[string]interface{})

	series := agg.GetSeries()
	if len(series) != 0 {
		if formatJSON {
			aggJSON["metrics"] = series
		} else {
			fmt.Fprintln(color.Output, fmt.Sprintf("=== %s ===", color.BlueString("Series")))
			j, _ := json.MarshalIndent(series, "", "  ")
			fmt.Println(string(j))
		}
	}

	sketches := agg.GetSketches()
	if len(sketches) != 0 {
		if formatJSON {
			aggJSON["sketches"] = sketches
		} else {
			fmt.Fprintln(color.Output, fmt.Sprintf("=== %s ===", color.BlueString("Sketches")))
			j, _ := json.MarshalIndent(sketches, "", "  ")
			fmt.Println(string(j))
		}
	}

	serviceChecks := agg.GetServiceChecks()
	if len(serviceChecks) != 0 {
		if formatJSON {
			aggJSON["service_checks"] = serviceChecks
		} else {
			fmt.Fprintln(color.Output, fmt.Sprintf("=== %s ===", color.BlueString("Service Checks")))
			j, _ := json.MarshalIndent(serviceChecks, "", "  ")
			fmt.Println(string(j))
		}
	}

	events := agg.GetEvents()
	if len(events) != 0 {
		if formatJSON {
			aggJSON["events"] = events
		} else {
			fmt.Fprintln(color.Output, fmt.Sprintf("=== %s ===", color.BlueString("Events")))
			j, _ := json.MarshalIndent(events, "", "  ")
			fmt.Println(string(j))
		}
	}

	if formatJSON {
		fmt.Fprintln(color.Output, fmt.Sprintf("=== %s ===", color.BlueString("JSON")))
		j, _ := json.MarshalIndent(aggJSON, "", "  ")
		fmt.Println(string(j))
	}
}
