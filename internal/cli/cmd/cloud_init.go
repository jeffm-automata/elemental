/*
Copyright © 2025-2026 SUSE LLC
SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/urfave/cli/v3"
)

type CloudInitFlags struct {
	Fetch             bool
	Stages            []string
	Finalize          bool
	Strict            bool
	ConfigPaths       []string
	DatasourceTimeout time.Duration
}

var CloudInitArgs CloudInitFlags

func NewCloudInitCommand(appName string, action func(context.Context, *cli.Command) error) *cli.Command {
	return &cli.Command{
		Name:  "cloud-init",
		Usage: "[EXPERIMENTAL] Apply cloud-init style configuration (cloud-config or yip documents)",
		UsageText: fmt.Sprintf("%s cloud-init [--fetch] [--stage STAGE]... [--finalize] [OPTIONS]\n\n"+
			"The steps requested are run in this order: fetch, stages (in the given order), finalize.", appName),
		Before: func(ctx context.Context, _ *cli.Command) (context.Context, error) {
			if !CloudInitArgs.Fetch && !CloudInitArgs.Finalize && len(CloudInitArgs.Stages) == 0 {
				return ctx, cli.Exit("Error: At least one of --fetch, --stage or --finalize flags must be specified.", 1)
			}

			return ctx, nil
		},
		Action: action,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name: "fetch",
				Usage: "Probe the datasources (NoCloud and ConfigDrive volumes) and prepare the user-data found " +
					"to be applied by the stages run in the current boot",
				Destination: &CloudInitArgs.Fetch,
			},
			&cli.StringSliceFlag{
				Name:        "stage",
				Usage:       "Stage to run, it can be repeated",
				Destination: &CloudInitArgs.Stages,
			},
			&cli.BoolFlag{
				Name:        "finalize",
				Usage:       "Mark the user-data fetched in the current boot as applied to this instance",
				Destination: &CloudInitArgs.Finalize,
			},
			&cli.BoolFlag{
				Name:        "strict",
				Usage:       "Fail if any error is found running a stage, by default errors are only logged",
				Destination: &CloudInitArgs.Strict,
			},
			&cli.StringSliceFlag{
				Name:        "config-path",
				Usage:       "File or directory to load yip documents from, it can be repeated",
				Destination: &CloudInitArgs.ConfigPaths,
				DefaultText: "/system/oem, /oem, /usr/local/cloud-config and the fetched user-data",
			},
			&cli.DurationFlag{
				Name:        "datasource-timeout",
				Usage:       "Time to wait for a datasource device to show up",
				Destination: &CloudInitArgs.DatasourceTimeout,
				Value:       5 * time.Second,
			},
		},
	}
}
