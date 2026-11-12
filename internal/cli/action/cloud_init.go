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

package action

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	cmdpkg "github.com/suse/elemental/v3/internal/cli/cmd"
	"github.com/suse/elemental/v3/internal/cloudinit"
	"github.com/suse/elemental/v3/pkg/sys"
)

func CloudInit(_ context.Context, cmd *cli.Command) error {
	args := &cmdpkg.CloudInitArgs

	if cmd.Root().Metadata == nil || cmd.Root().Metadata["system"] == nil {
		return fmt.Errorf("error setting up initial configuration")
	}
	system := cmd.Root().Metadata["system"].(*sys.System)
	logger := system.Logger()

	cfg := cloudinit.NewConfig()
	cfg.Strict = args.Strict
	cfg.DatasourceTimeout = args.DatasourceTimeout
	if len(args.ConfigPaths) > 0 {
		cfg.ConfigPaths = args.ConfigPaths
	}

	ci := cloudinit.New(system, cfg)

	if args.Fetch {
		if err := ci.Fetch(); err != nil {
			logger.Error("Fetching cloud-init user-data failed")
			return err
		}
	}

	for _, stage := range args.Stages {
		if err := ci.RunStage(stage); err != nil {
			logger.Error("Running cloud-init stage '%s' failed", stage)
			return err
		}
	}

	if args.Finalize {
		if err := ci.Finalize(); err != nil {
			logger.Error("Finalizing cloud-init failed")
			return err
		}
	}

	return nil
}
