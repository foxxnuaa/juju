// Copyright 2015 Canonical Ltd.
// Licensed under the LGPLv3, see LICENCE file for details.

package service_test

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/juju/testing"
	jc "github.com/juju/testing/checkers"
	gc "gopkg.in/check.v1"

	"github.com/juju/juju/service"
	"github.com/juju/juju/service/common"
	"github.com/juju/juju/service/upstart"
	"github.com/juju/juju/service/windows"
)

type serviceSuite struct {
	testing.IsolationSuite
}

var _ = gc.Suite(&serviceSuite{})

func (*serviceSuite) TestDiscoverService(c *gc.C) {
	name := "a-service"
	conf := common.Conf{
		Desc:      "some service",
		ExecStart: "<do something>",
	}
	svc, err := service.DiscoverService(name, conf)
	c.Assert(err, jc.ErrorIsNil)

	switch runtime.GOOS {
	case "linux":
		c.Check(svc, gc.FitsTypeOf, &upstart.Service{})
		conf.InitDir = "/etc/init"
	case "windows":
		c.Check(svc, gc.FitsTypeOf, &windows.Service{})
	default:
		c.Errorf("unrecognized os %q", runtime.GOOS)
	}
	c.Check(svc.Name(), gc.Equals, "a-service")
	c.Check(svc.Conf(), jc.DeepEquals, conf)
}

func (*serviceSuite) TestListServicesCommand(c *gc.C) {
	cmd := service.ListServicesCommand()

	line := `if [[ "$(cat /proc/1/cmdline)" == "%s" ]]; then %s`
	upstart := `sudo initctl list | awk '{print $1}' | sort | uniq`
	systemd := `/bin/systemctl list-unit-files --no-legend --no-page -t service` +
		` | grep -o -P '^\w[\S]*(?=\.service)'`

	lines := []string{
		fmt.Sprintf(line, "/sbin/init", upstart),
		fmt.Sprintf(line, "/sbin/upstart", upstart),
		fmt.Sprintf(line, "/sbin/systemd", systemd),
		fmt.Sprintf(line, "/bin/systemd", systemd),
		fmt.Sprintf(line, "/lib/systemd/systemd", systemd),
	}
	
	/* We expect the command sequence to start with an if <command>
	   then each command to be prefixed with elif and the whole
	   list to be terminated by "else exit 1". The particular commands
	   don't have a required order, so we accept any order. */
	cmds := strings.Split(cmd, "\n")
	foundLines := 0
	for i := range cmds {
		cmdline := ""
		switch i {
			case 0:
				c.Check(cmds[i][0:3], gc.Equals, "if ")
				cmdline = cmds[i]
			case len(cmds) - 2:
				c.Check(cmds[i], gc.Equals, "else exit 1")
			case len(cmds) - 1:
				c.Check(cmds[i], gc.Equals, "fi")
			default:
				c.Check(cmds[i][0:5], gc.Equals, "elif ")
				cmdline = cmds[i][2:]
		}
		if cmdline != "" {
			ok := false
			for _, testLine := range(lines) {
				if testLine == cmdline {
					ok = true
					foundLines++
					break
				}
			}
			c.Check(ok, gc.Equals, true)
		}
	}
	c.Check(foundLines, gc.Equals, len(lines))
	return
}
