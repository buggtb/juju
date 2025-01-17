// Copyright 2012, 2013 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package deployer_test

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"

	"github.com/juju/os/series"
	jc "github.com/juju/testing/checkers"
	"github.com/juju/utils/arch"
	"github.com/juju/version"
	gc "gopkg.in/check.v1"
	"gopkg.in/juju/names.v3"

	"github.com/juju/juju/agent"
	"github.com/juju/juju/agent/tools"
	svctesting "github.com/juju/juju/service/common/testing"
	"github.com/juju/juju/service/upstart"
	"github.com/juju/juju/state/multiwatcher"
	"github.com/juju/juju/testing"
	coretools "github.com/juju/juju/tools"
	jujuversion "github.com/juju/juju/version"
	"github.com/juju/juju/worker/deployer"
)

var quote, cmdSuffix string

func init() {
	quote = "'"
	if runtime.GOOS == "windows" {
		cmdSuffix = ".exe"
		quote = `"`
	}
}

type SimpleContextSuite struct {
	SimpleToolsFixture
}

var _ = gc.Suite(&SimpleContextSuite{})

func (s *SimpleContextSuite) SetUpTest(c *gc.C) {
	s.SimpleToolsFixture.SetUp(c, c.MkDir())
}

func (s *SimpleContextSuite) TearDownTest(c *gc.C) {
	s.SimpleToolsFixture.TearDown(c)
}

func (s *SimpleContextSuite) TestDeployRecall(c *gc.C) {
	mgr0 := s.getContext(c)
	units, err := mgr0.DeployedUnits()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(units, gc.HasLen, 0)
	s.assertUpstartCount(c, 0)

	err = mgr0.DeployUnit("foo/123", "some-password")
	c.Assert(err, jc.ErrorIsNil)
	units, err = mgr0.DeployedUnits()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(units, gc.DeepEquals, []string{"foo/123"})
	s.assertUpstartCount(c, 1)
	s.checkUnitInstalled(c, "foo/123", "some-password")

	err = mgr0.RecallUnit("foo/123")
	c.Assert(err, jc.ErrorIsNil)
	units, err = mgr0.DeployedUnits()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(units, gc.HasLen, 0)
	s.assertUpstartCount(c, 0)
	s.checkUnitRemoved(c, "foo/123")
}

func (s *SimpleContextSuite) TestOldDeployedUnitsCanBeRecalled(c *gc.C) {
	// After r1347 deployer tag is no longer part of the upstart conf filenames,
	// now only the units' tags are used. This change is with the assumption only
	// one deployer will be running on a machine (in the machine agent as a task,
	// unlike before where there was one in the unit agent as well).
	// This test ensures units deployed previously (or their upstart confs more
	// specifically) can be detected and recalled by the deployer.

	manager := s.getContext(c)

	// No deployed units at first.
	units, err := manager.DeployedUnits()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(units, gc.HasLen, 0)
	s.assertUpstartCount(c, 0)

	// Trying to recall any units will fail.
	err = manager.RecallUnit("principal/1")
	c.Assert(err, gc.ErrorMatches, `unit "principal/1" is not deployed`)

	// Simulate some previously deployed units with the old
	// upstart conf filename format (+deployer tags).
	s.injectUnit(c, "jujud-machine-0:unit-mysql-0", "unit-mysql-0")
	s.assertUpstartCount(c, 1)
	s.injectUnit(c, "jujud-unit-wordpress-0:unit-nrpe-0", "unit-nrpe-0")
	s.assertUpstartCount(c, 2)

	// Make sure we can discover them.
	units, err = manager.DeployedUnits()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(units, gc.HasLen, 2)
	sort.Strings(units)
	c.Assert(units, gc.DeepEquals, []string{"mysql/0", "nrpe/0"})

	// Deploy some units.
	err = manager.DeployUnit("principal/1", "some-password")
	c.Assert(err, jc.ErrorIsNil)
	s.checkUnitInstalled(c, "principal/1", "some-password")
	s.assertUpstartCount(c, 3)
	err = manager.DeployUnit("subordinate/2", "fake-password")
	c.Assert(err, jc.ErrorIsNil)
	s.checkUnitInstalled(c, "subordinate/2", "fake-password")
	s.assertUpstartCount(c, 4)

	// Verify the newly deployed units are also discoverable.
	units, err = manager.DeployedUnits()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(units, gc.HasLen, 4)
	sort.Strings(units)
	c.Assert(units, gc.DeepEquals, []string{"mysql/0", "nrpe/0", "principal/1", "subordinate/2"})

	// Recall all of them - should work ok.
	unitCount := 4
	for _, unitName := range units {
		err = manager.RecallUnit(unitName)
		c.Assert(err, jc.ErrorIsNil)
		unitCount--
		s.checkUnitRemoved(c, unitName)
		s.assertUpstartCount(c, unitCount)
	}

	// Verify they're no longer discoverable.
	units, err = manager.DeployedUnits()
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(units, gc.HasLen, 0)
}

type SimpleToolsFixture struct {
	dataDir  string
	logDir   string
	origPath string
	binDir   string

	data *svctesting.FakeServiceData
}

var fakeJujud = "#!/bin/bash --norc\n# fake-jujud\nexit 0\n"

func (fix *SimpleToolsFixture) SetUp(c *gc.C, dataDir string) {
	fix.dataDir = dataDir
	fix.logDir = c.MkDir()
	current := version.Binary{
		Number: jujuversion.Current,
		Arch:   arch.HostArch(),
		Series: series.MustHostSeries(),
	}
	toolsDir := tools.SharedToolsDir(fix.dataDir, current)
	err := os.MkdirAll(toolsDir, 0755)
	c.Assert(err, jc.ErrorIsNil)
	jujudPath := filepath.Join(toolsDir, "jujud")
	err = ioutil.WriteFile(jujudPath, []byte(fakeJujud), 0755)
	c.Assert(err, jc.ErrorIsNil)
	toolsPath := filepath.Join(toolsDir, "downloaded-tools.txt")
	testTools := coretools.Tools{Version: current, URL: "http://testing.invalid/tools"}
	data, err := json.Marshal(testTools)
	c.Assert(err, jc.ErrorIsNil)
	err = ioutil.WriteFile(toolsPath, data, 0644)
	c.Assert(err, jc.ErrorIsNil)
	fix.binDir = c.MkDir()
	fix.origPath = os.Getenv("PATH")
	os.Setenv("PATH", fix.binDir+":"+fix.origPath)
	fix.makeBin(c, "status", `echo "blah stop/waiting"`)
	fix.makeBin(c, "stopped-status", `echo "blah stop/waiting"`)
	fix.makeBin(c, "started-status", `echo "blah start/running, process 666"`)
	fix.makeBin(c, "start", "cp $(which started-status) $(which status)")
	fix.makeBin(c, "stop", "cp $(which stopped-status) $(which status)")

	fix.data = svctesting.NewFakeServiceData()
}

func (fix *SimpleToolsFixture) TearDown(c *gc.C) {
	os.Setenv("PATH", fix.origPath)
}

func (fix *SimpleToolsFixture) makeBin(c *gc.C, name, script string) {
	path := filepath.Join(fix.binDir, name)
	err := ioutil.WriteFile(path, []byte("#!/bin/bash --norc\n"+script), 0755)
	c.Assert(err, jc.ErrorIsNil)
}

func (fix *SimpleToolsFixture) assertUpstartCount(c *gc.C, count int) {
	c.Assert(fix.data.InstalledNames(), gc.HasLen, count)
}

func (fix *SimpleToolsFixture) getContext(c *gc.C) *deployer.SimpleContext {
	config := agentConfig(names.NewMachineTag("99"), fix.dataDir, fix.logDir)
	return deployer.NewTestSimpleContext(config, fix.logDir, fix.data)
}

func (fix *SimpleToolsFixture) getContextForMachine(c *gc.C, machineTag names.Tag) *deployer.SimpleContext {
	config := agentConfig(machineTag, fix.dataDir, fix.logDir)
	return deployer.NewTestSimpleContext(config, fix.logDir, fix.data)
}

func (fix *SimpleToolsFixture) paths(tag names.Tag) (agentDir, toolsDir string) {
	agentDir = agent.Dir(fix.dataDir, tag)
	toolsDir = tools.ToolsDir(fix.dataDir, tag.String())
	return
}

func (fix *SimpleToolsFixture) checkUnitInstalled(c *gc.C, name, password string) {
	tag := names.NewUnitTag(name)

	svcName := "jujud-" + tag.String()
	assertContains(c, fix.data.InstalledNames(), svcName)

	svcConf := fix.data.GetInstalled(svcName).Conf()
	// TODO(ericsnow) For now we just use upstart serialization.
	uconfData, err := upstart.Serialize(svcName, svcConf)
	c.Assert(err, jc.ErrorIsNil)
	uconf := string(uconfData)

	regex := regexp.MustCompile("(?m)(?:^\\s)*exec\\s.+$")
	execs := regex.FindAllString(uconf, -1)

	if nil == execs {
		c.Fatalf("no command found in conf:\n%s", uconf)
	} else if 1 > len(execs) {
		c.Fatalf("Test is not built to handle more than one exec line.")
	}

	_, toolsDir := fix.paths(tag)
	jujudPath := filepath.Join(toolsDir, "jujud"+cmdSuffix)

	logPath := filepath.Join(fix.logDir, tag.String()+".log")

	for _, pat := range []string{
		"^exec " + quote + jujudPath + quote + " unit ",
		" --unit-name " + name + " ",
		" >> " + logPath + " 2>&1$",
	} {
		match, err := regexp.MatchString(pat, execs[0])
		c.Assert(err, jc.ErrorIsNil)
		if !match {
			c.Fatalf("failed to match:\n%s\nin:\n%s", pat, execs[0])
		}
	}

	conf, err := agent.ReadConfig(agent.ConfigPath(fix.dataDir, tag))
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(conf.Tag(), gc.Equals, tag)
	c.Assert(conf.DataDir(), gc.Equals, fix.dataDir)

	jujudData, err := ioutil.ReadFile(jujudPath)
	c.Assert(err, jc.ErrorIsNil)
	c.Assert(string(jujudData), gc.Equals, fakeJujud)
}

func (fix *SimpleToolsFixture) checkUnitRemoved(c *gc.C, name string) {
	assertNotContains(c, fix.data.InstalledNames(), name)

	tag := names.NewUnitTag(name)
	agentDir, toolsDir := fix.paths(tag)
	for _, path := range []string{agentDir, toolsDir} {
		_, err := ioutil.ReadFile(path)
		if err == nil {
			c.Logf("Warning: %q not removed as expected", path)
		} else {
			c.Assert(err, jc.Satisfies, os.IsNotExist)
		}
	}
}

func (fix *SimpleToolsFixture) injectUnit(c *gc.C, name, unitTag string) {
	fix.data.SetStatus(name, "installed")

	toolsDir := filepath.Join(fix.dataDir, "tools", unitTag)
	err := os.MkdirAll(toolsDir, 0755)
	c.Assert(err, jc.ErrorIsNil)
}

type mockConfig struct {
	agent.Config
	tag               names.Tag
	datadir           string
	logdir            string
	upgradedToVersion version.Number
	jobs              []multiwatcher.MachineJob
}

func (mock *mockConfig) Tag() names.Tag {
	return mock.tag
}

func (mock *mockConfig) DataDir() string {
	return mock.datadir
}

func (mock *mockConfig) LogDir() string {
	return mock.logdir
}

func (mock *mockConfig) Jobs() []multiwatcher.MachineJob {
	return mock.jobs
}

func (mock *mockConfig) UpgradedToVersion() version.Number {
	return mock.upgradedToVersion
}

func (mock *mockConfig) WriteUpgradedToVersion(newVersion version.Number) error {
	mock.upgradedToVersion = newVersion
	return nil
}

func (mock *mockConfig) Model() names.ModelTag {
	return testing.ModelTag
}

func (mock *mockConfig) Controller() names.ControllerTag {
	return testing.ControllerTag
}

func (mock *mockConfig) CACert() string {
	return testing.CACert
}

func (mock *mockConfig) Value(_ string) string {
	return ""
}

func agentConfig(tag names.Tag, datadir, logdir string) agent.Config {
	return &mockConfig{tag: tag, datadir: datadir, logdir: logdir}
}

// assertContains asserts a needle is contained within haystack
func assertContains(c *gc.C, haystack []string, needle string) {
	c.Assert(contains(haystack, needle), jc.IsTrue)
}

// assertNotContains asserts a needle is not contained within haystack
func assertNotContains(c *gc.C, haystack []string, needle string) {
	c.Assert(contains(haystack, needle), gc.Not(jc.IsTrue))
}

func contains(haystack []string, needle string) bool {
	for _, e := range haystack {
		if e == needle {
			return true
		}
	}
	return false
}
