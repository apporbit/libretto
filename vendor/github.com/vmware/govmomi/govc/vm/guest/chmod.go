/*
Copyright (c) 2014-2015 VMware, Inc. All Rights Reserved.

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

package guest

import (
	"context"
	"flag"

	"github.com/vmware/govmomi/govc/cli"
)

type chmod struct {
	*GuestFlag
	*FileAttrFlag
}

func init() {
	cli.Register("guest.chmod", &chmod{})
}

func (cmd *chmod) Register(ctx context.Context, f *flag.FlagSet) {
	cmd.GuestFlag, ctx = newGuestFlag(ctx)
	cmd.GuestFlag.Register(ctx, f)
	cmd.FileAttrFlag, ctx = newFileAttrFlag(ctx)
	cmd.FileAttrFlag.Register(ctx, f)
}

func (cmd *chmod) Process(ctx context.Context) error {
	if err := cmd.GuestFlag.Process(ctx); err != nil {
		return err
	}
	if err := cmd.FileAttrFlag.Process(ctx); err != nil {
		return err
	}
	return nil
}

func (cmd *chmod) Run(ctx context.Context, f *flag.FlagSet) error {
	m, err := cmd.FileManager()
	if err != nil {
		return err
	}

	return m.ChangeFileAttributes(ctx, cmd.Auth(), f.Arg(0), cmd.Attr())
}
