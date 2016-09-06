// Copyright 2014 The rkt Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//+build linux

package common

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/coreos/rkt/pkg/acl"
	stage1commontypes "github.com/coreos/rkt/stage1/common/types"

	"github.com/appc/spec/schema"
	"github.com/appc/spec/schema/types"
	"github.com/coreos/go-systemd/unit"
	"github.com/hashicorp/errwrap"

	"github.com/coreos/rkt/common"
	"github.com/coreos/rkt/pkg/fileutil"
	"github.com/coreos/rkt/pkg/user"
)

const (
	// FlavorFile names the file storing the pod's flavor
	FlavorFile    = "flavor"
	SharedVolPerm = os.FileMode(0755)
)

var (
	defaultEnv = map[string]string{
		"PATH":    "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"SHELL":   "/bin/sh",
		"USER":    "root",
		"LOGNAME": "root",
		"HOME":    "/root",
	}
)

type Stage1InsecureOptions struct {
	DisablePaths        bool
	DisableCapabilities bool
	DisableSeccomp      bool
}

// execEscape uses Golang's string quoting for ", \, \n, and regex for special cases
func execEscape(i int, str string) string {
	escapeMap := map[string]string{
		`'`: `\`,
	}

	if i > 0 { // These are escaped only after the first argument
		escapeMap[`$`] = `$`
	}

	escArg := fmt.Sprintf("%q", str)
	for k := range escapeMap {
		reStr := `([` + regexp.QuoteMeta(k) + `])`
		re := regexp.MustCompile(reStr)
		escArg = re.ReplaceAllStringFunc(escArg, func(s string) string {
			escaped := escapeMap[s] + s
			return escaped
		})
	}
	return escArg
}

// quoteExec returns an array of quoted strings appropriate for systemd execStart usage
func quoteExec(exec []string) string {
	if len(exec) == 0 {
		// existing callers always include at least the binary so this shouldn't occur.
		panic("empty exec")
	}

	var qexec []string
	for i, arg := range exec {
		escArg := execEscape(i, arg)
		qexec = append(qexec, escArg)
	}
	return strings.Join(qexec, " ")
}

func writeAppReaper(p *stage1commontypes.Pod, appName string, appRootDirectory string, binPath string) error {
	opts := []*unit.UnitOption{
		unit.NewUnitOption("Unit", "Description", fmt.Sprintf("%s Reaper", appName)),
		unit.NewUnitOption("Unit", "DefaultDependencies", "false"),
		unit.NewUnitOption("Unit", "StopWhenUnneeded", "yes"),
		unit.NewUnitOption("Unit", "Wants", "shutdown.service"),
		unit.NewUnitOption("Unit", "After", "shutdown.service"),
		unit.NewUnitOption("Unit", "Conflicts", "exit.target"),
		unit.NewUnitOption("Unit", "Conflicts", "halt.target"),
		unit.NewUnitOption("Unit", "Conflicts", "poweroff.target"),
		unit.NewUnitOption("Service", "RemainAfterExit", "yes"),
		unit.NewUnitOption("Service", "ExecStop", fmt.Sprintf("/reaper.sh \"%s\" \"%s\" \"%s\"", appName, appRootDirectory, binPath)),
	}

	unitsPath := filepath.Join(common.Stage1RootfsPath(p.Root), UnitsDir)
	file, err := os.OpenFile(filepath.Join(unitsPath, fmt.Sprintf("reaper-%s.service", appName)), os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return errwrap.Wrap(errors.New("failed to create service unit file"), err)
	}
	defer file.Close()

	if _, err = io.Copy(file, unit.Serialize(opts)); err != nil {
		return errwrap.Wrap(errors.New("failed to write service unit file"), err)
	}

	return nil
}

// SetJournalPermissions sets ACLs and permissions so the rkt group can access
// the pod's logs
func SetJournalPermissions(p *stage1commontypes.Pod) error {
	s1 := common.Stage1ImagePath(p.Root)

	rktgid, err := common.LookupGid(common.RktGroup)
	if err != nil {
		return fmt.Errorf("group %q not found", common.RktGroup)
	}

	journalPath := filepath.Join(s1, "rootfs", "var", "log", "journal")
	if err := os.MkdirAll(journalPath, os.FileMode(0755)); err != nil {
		return errwrap.Wrap(errors.New("error creating journal dir"), err)
	}

	a, err := acl.InitACL()
	if err != nil {
		return err
	}
	defer a.Free()

	if err := a.ParseACL(fmt.Sprintf("g:%d:r-x,m:r-x", rktgid)); err != nil {
		return errwrap.Wrap(errors.New("error parsing ACL string"), err)
	}

	if err := a.AddBaseEntries(journalPath); err != nil {
		return errwrap.Wrap(errors.New("error adding base ACL entries"), err)
	}

	if err := a.Valid(); err != nil {
		return err
	}

	if err := a.SetFileACLDefault(journalPath); err != nil {
		return errwrap.Wrap(fmt.Errorf("error setting default ACLs on %q", journalPath), err)
	}

	return nil
}

func generateGidArg(gid int, supplGid []int) string {
	arg := []string{strconv.Itoa(gid)}
	for _, sg := range supplGid {
		arg = append(arg, strconv.Itoa(sg))
	}
	return strings.Join(arg, ",")
}

// findHostPort returns the port number on the host that corresponds to an
// image manifest port identified by name
func findHostPort(pm schema.PodManifest, name types.ACName) uint {
	var port uint
	for _, p := range pm.Ports {
		if p.Name == name {
			port = p.HostPort
		}
	}
	return port
}

// generateSysusers generates systemd sysusers files for a given app so that
// corresponding entries in /etc/passwd and /etc/group are created in stage1.
// This is needed to use the "User=" and "Group=" options in the systemd
// service files of apps.
// If there're several apps defining the same UIDs/GIDs, systemd will take care
// of only generating one /etc/{passwd,group} entry
func generateSysusers(p *stage1commontypes.Pod, ra *schema.RuntimeApp, uid_ int, gid_ int, uidRange *user.UidRange) error {
	var toShift []string

	app := ra.App
	appName := ra.Name

	sysusersDir := path.Join(common.Stage1RootfsPath(p.Root), "usr/lib/sysusers.d")
	toShift = append(toShift, sysusersDir)
	if err := os.MkdirAll(sysusersDir, 0755); err != nil {
		return err
	}

	gids := append(app.SupplementaryGIDs, gid_)

	// Create the Unix user and group
	var sysusersConf []string

	for _, g := range gids {
		groupname := "gen" + strconv.Itoa(g)
		sysusersConf = append(sysusersConf, fmt.Sprintf("g %s %d\n", groupname, g))
	}

	username := "gen" + strconv.Itoa(uid_)
	sysusersConf = append(sysusersConf, fmt.Sprintf("u %s %d \"%s\"\n", username, uid_, username))

	sysusersFile := path.Join(common.Stage1RootfsPath(p.Root), "usr/lib/sysusers.d", ServiceUnitName(appName)+".conf")
	toShift = append(toShift, sysusersFile)
	if err := ioutil.WriteFile(sysusersFile, []byte(strings.Join(sysusersConf, "\n")), 0640); err != nil {
		return err
	}

	if err := shiftFiles(toShift, uidRange); err != nil {
		return err
	}

	return nil
}

// lookupPathInsideApp returns the path (relative to the app rootfs) of the
// given binary. It will look up on "paths" (also relative to the app rootfs)
// and evaluate possible symlinks to check if the resulting path is actually
// executable.
func lookupPathInsideApp(bin string, paths string, appRootfs string, workDir string) (string, error) {
	pathsArr := filepath.SplitList(paths)
	var appPathsArr []string
	for _, p := range pathsArr {
		if !filepath.IsAbs(p) {
			p = filepath.Join(workDir, p)
		}
		appPathsArr = append(appPathsArr, filepath.Join(appRootfs, p))
	}
	for _, path := range appPathsArr {
		binPath := filepath.Join(path, bin)
		stage2Path := strings.TrimPrefix(binPath, appRootfs)
		binRealPath, err := EvaluateSymlinksInsideApp(appRootfs, stage2Path)
		if err != nil {
			return "", errwrap.Wrap(fmt.Errorf("could not evaluate path %v", stage2Path), err)
		}
		binRealPath = filepath.Join(appRootfs, binRealPath)
		if fileutil.IsExecutable(binRealPath) {
			// The real path is executable, return the path relative to the app
			return stage2Path, nil
		}
	}
	return "", fmt.Errorf("unable to find %q in %q", bin, paths)
}

// appSearchPaths returns a list of paths where we should search for
// non-absolute exec binaries
func appSearchPaths(p *stage1commontypes.Pod, workDir string, app types.App) []string {
	appEnv := app.Environment

	if imgPath, ok := appEnv.Get("PATH"); ok {
		return strings.Split(imgPath, ":")
	}

	// emulate exec(3) behavior, first check working directory and then the
	// list of directories returned by confstr(_CS_PATH). That's typically
	// "/bin:/usr/bin" so let's use that.
	return []string{workDir, "/bin", "/usr/bin"}
}

// findBinPath takes a binary path and returns a the absolute path of the
// binary relative to the app rootfs. This can be passed to ExecStart on the
// app's systemd service file directly.
func findBinPath(p *stage1commontypes.Pod, appName types.ACName, app types.App, workDir string, bin string) (string, error) {
	var binPath string
	switch {
	// absolute path, just use it
	case filepath.IsAbs(bin):
		binPath = bin
	// non-absolute path containing a slash, look in the working dir
	case strings.Contains(bin, "/"):
		binPath = filepath.Join(workDir, bin)
	// filename, search in the app's $PATH
	default:
		absRoot, err := filepath.Abs(p.Root)
		if err != nil {
			return "", errwrap.Wrap(errors.New("could not get pod's root absolute path"), err)
		}
		appRootfs := common.AppRootfsPath(absRoot, appName)
		appPathDirs := appSearchPaths(p, workDir, app)
		appPath := strings.Join(appPathDirs, ":")

		binPath, err = lookupPathInsideApp(bin, appPath, appRootfs, workDir)
		if err != nil {
			return "", errwrap.Wrap(fmt.Errorf("error looking up %q", bin), err)
		}
	}

	return binPath, nil
}

// shiftFiles shifts filesToshift by the amounts specified in uidRange
func shiftFiles(filesToShift []string, uidRange *user.UidRange) error {
	if uidRange.Shift != 0 && uidRange.Count != 0 {
		for _, f := range filesToShift {
			if err := os.Chown(f, int(uidRange.Shift), int(uidRange.Shift)); err != nil {
				return err
			}
		}
	}
	return nil
}

// generateDeviceAllows generates a DeviceAllow= line for an app.
// To make it work, the path needs to start with "/dev" but the device won't
// exist inside the container. So for a given mount, if the volume is a device
// node, we create a symlink to its target in "/rkt/volumes". Later,
// prepare-app will copy those to "/dev/.rkt/" so that's what we use in the
// DeviceAllow= line.
func generateDeviceAllows(root string, appName types.ACName, mountPoints []types.MountPoint, mounts []mountWrapper, vols map[types.ACName]types.Volume, uidRange *user.UidRange) ([]string, error) {
	var devAllow []string

	rktVolumeLinksPath := filepath.Join(root, "rkt", "volumes")
	if err := os.MkdirAll(rktVolumeLinksPath, 0600); err != nil {
		return nil, err
	}
	if err := shiftFiles([]string{rktVolumeLinksPath}, uidRange); err != nil {
		return nil, err
	}

	for _, m := range mounts {
		v := vols[m.Volume]
		if v.Kind != "host" {
			continue
		}
		if fileutil.IsDeviceNode(v.Source) {
			mode := "r"
			if !IsMountReadOnly(v, mountPoints) {
				mode += "w"
			}

			tgt := filepath.Join(common.RelAppRootfsPath(appName), m.Path)
			// the DeviceAllow= line needs the link path in /dev/.rkt/
			linkRel := filepath.Join("/dev/.rkt", v.Name.String())
			// the real link should be in /rkt/volumes for now
			link := filepath.Join(rktVolumeLinksPath, v.Name.String())

			err := os.Symlink(tgt, link)
			// if the link already exists, we don't need to do anything
			if err != nil && !os.IsExist(err) {
				return nil, err
			}

			devAllow = append(devAllow, linkRel+" "+mode)
		}
	}

	return devAllow, nil
}

// parseUserGroup parses the User and Group fields of an App and returns its
// UID and GID.
// The User and Group fields accept several formats:
//   1. the hardcoded string "root"
//   2. a path
//   3. a number
//   4. a name in reference to /etc/{group,passwd} in the image
// See https://github.com/appc/spec/blob/master/spec/aci.md#image-manifest-schema
func parseUserGroup(p *stage1commontypes.Pod, ra *schema.RuntimeApp, uidRange *user.UidRange) (int, int, error) {
	var uidResolver, gidResolver user.Resolver
	var uid, gid int
	var err error

	root := common.AppRootfsPath(p.Root, ra.Name)

	uidResolver, err = user.NumericIDs(ra.App.User)
	if err != nil {
		uidResolver, err = user.IDsFromStat(root, ra.App.User, uidRange)
	}

	if err != nil {
		uidResolver, err = user.IDsFromEtc(root, ra.App.User, "")
	}

	if err != nil { // give up
		return -1, -1, errwrap.Wrap(fmt.Errorf("invalid user %q", ra.App.User), err)
	}

	if uid, _, err = uidResolver.IDs(); err != nil {
		return -1, -1, errwrap.Wrap(fmt.Errorf("failed to configure user %q", ra.App.User), err)
	}

	gidResolver, err = user.NumericIDs(ra.App.Group)
	if err != nil {
		gidResolver, err = user.IDsFromStat(root, ra.App.Group, uidRange)
	}

	if err != nil {
		gidResolver, err = user.IDsFromEtc(root, "", ra.App.Group)
	}

	if err != nil { // give up
		return -1, -1, errwrap.Wrap(fmt.Errorf("invalid group %q", ra.App.Group), err)
	}

	if _, gid, err = gidResolver.IDs(); err != nil {
		return -1, -1, errwrap.Wrap(fmt.Errorf("failed to configure group %q", ra.App.Group), err)
	}

	return uid, gid, nil
}

// writeEnvFile creates an environment file for given app name, the minimum
// required environment variables by the appc spec will be set to sensible
// defaults here if they're not provided by env.
func writeEnvFile(p *stage1commontypes.Pod, env types.Environment, appName types.ACName, uidRange *user.UidRange, separator byte, envFilePath string) error {
	ef := bytes.Buffer{}

	for dk, dv := range defaultEnv {
		if _, exists := env.Get(dk); !exists {
			fmt.Fprintf(&ef, "%s=%s%c", dk, dv, separator)
		}
	}

	for _, e := range env {
		fmt.Fprintf(&ef, "%s=%s%c", e.Name, e.Value, separator)
	}

	if err := ioutil.WriteFile(envFilePath, ef.Bytes(), 0644); err != nil {
		return err
	}

	if err := shiftFiles([]string{envFilePath}, uidRange); err != nil {
		return err
	}

	return nil
}

// EvaluateSymlinksInsideApp tries to resolve symlinks within the path.
// It returns the actual path relative to the app rootfs for the given path.
func EvaluateSymlinksInsideApp(appRootfs, path string) (string, error) {
	link := appRootfs

	paths := strings.Split(path, "/")
	for i, p := range paths {
		next := filepath.Join(link, p)

		if !strings.HasPrefix(next, appRootfs) {
			return "", fmt.Errorf("path escapes app's root: %q", path)
		}

		fi, err := os.Lstat(next)
		if err != nil {
			if os.IsNotExist(err) {
				link = filepath.Join(append([]string{link}, paths[i:]...)...)
				break
			}
			return "", err
		}

		if fi.Mode()&os.ModeType != os.ModeSymlink {
			link = filepath.Join(link, p)
			continue
		}

		// Evaluate the symlink.
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}

		if filepath.IsAbs(target) {
			link = filepath.Join(appRootfs, target)
		} else {
			link = filepath.Join(link, target)
		}

		if !strings.HasPrefix(link, appRootfs) {
			return "", fmt.Errorf("symlink %q escapes app's root with value %q", next, target)
		}
	}

	return strings.TrimPrefix(link, appRootfs), nil
}

// appToNspawnArgs transforms the given app manifest, with the given associated
// app name, into a subset of applicable systemd-nspawn argument
func appToNspawnArgs(p *stage1commontypes.Pod, ra *schema.RuntimeApp, insecureOptions Stage1InsecureOptions) ([]string, error) {
	var args []string
	appName := ra.Name
	app := ra.App

	sharedVolPath := common.SharedVolumesPath(p.Root)
	if err := os.MkdirAll(sharedVolPath, SharedVolPerm); err != nil {
		return nil, errwrap.Wrap(errors.New("could not create shared volumes directory"), err)
	}
	if err := os.Chmod(sharedVolPath, SharedVolPerm); err != nil {
		return nil, errwrap.Wrap(fmt.Errorf("could not change permissions of %q", sharedVolPath), err)
	}

	vols := make(map[types.ACName]types.Volume)
	for _, v := range p.Manifest.Volumes {
		vols[v.Name] = v
	}

	imageManifest := p.Images[appName.String()]
	mounts := GenerateMounts(ra, vols, imageManifest)
	for _, m := range mounts {
		vol := vols[m.Volume]

		shPath := filepath.Join(sharedVolPath, vol.Name.String())

		absRoot, err := filepath.Abs(p.Root) // Absolute path to the pod's rootfs.
		if err != nil {
			return nil, errwrap.Wrap(errors.New("could not get pod's root absolute path"), err)
		}

		appRootfs := common.AppRootfsPath(absRoot, appName)

		// TODO(yifan): This is a temporary fix for systemd-nspawn not handling symlink mounts well.
		// Could be removed when https://github.com/systemd/systemd/issues/2860 is resolved, and systemd
		// version is bumped.
		mntPath, err := EvaluateSymlinksInsideApp(appRootfs, m.Path)
		if err != nil {
			return nil, errwrap.Wrap(fmt.Errorf("could not evaluate path %v", m.Path), err)
		}
		mntAbsPath := filepath.Join(appRootfs, mntPath)

		if err := PrepareMountpoints(shPath, mntAbsPath, &vol, m.DockerImplicit); err != nil {
			return nil, err
		}

		opt := make([]string, 6)

		if IsMountReadOnly(vol, app.MountPoints) {
			opt[0] = "--bind-ro="
		} else {
			opt[0] = "--bind="
		}

		switch vol.Kind {
		case "host":
			opt[1] = vol.Source
		case "empty":
			opt[1] = filepath.Join(common.SharedVolumesPath(absRoot), vol.Name.String())
		default:
			return nil, fmt.Errorf(`invalid volume kind %q. Must be one of "host" or "empty"`, vol.Kind)
		}
		opt[2] = ":"
		opt[3] = filepath.Join(common.RelAppRootfsPath(appName), mntPath)
		opt[4] = ":"

		// If Recursive is not set, default to recursive.
		recursive := true
		if vol.Recursive != nil {
			recursive = *vol.Recursive
		}

		// rbind/norbind options exist since systemd-nspawn v226
		if recursive {
			opt[5] = "rbind"
		} else {
			opt[5] = "norbind"
		}
		args = append(args, strings.Join(opt, ""))
	}

	if !insecureOptions.DisableCapabilities {
		capabilitiesStr, err := getAppCapabilities(app.Isolators)
		if err != nil {
			return nil, err
		}
		capList := strings.Join(capabilitiesStr, ",")
		args = append(args, "--capability="+capList)
	}

	return args, nil
}

// PodToNspawnArgs renders a prepared Pod as a systemd-nspawn
// argument list ready to be executed
func PodToNspawnArgs(p *stage1commontypes.Pod, insecureOptions Stage1InsecureOptions) ([]string, error) {
	args := []string{
		"--uuid=" + p.UUID.String(),
		"--machine=" + GetMachineID(p),
		"--directory=" + common.Stage1RootfsPath(p.Root),
	}

	for i := range p.Manifest.Apps {
		aa, err := appToNspawnArgs(p, &p.Manifest.Apps[i], insecureOptions)
		if err != nil {
			return nil, err
		}
		args = append(args, aa...)
	}

	if insecureOptions.DisableCapabilities {
		args = append(args, "--capability=all")
	}

	return args, nil
}

// GetFlavor populates a flavor string based on the flavor itself and respectively the systemd version
// If the systemd version couldn't be guessed, it will be set to 0.
func GetFlavor(p *stage1commontypes.Pod) (flavor string, systemdVersion int, err error) {
	flavor, err = os.Readlink(filepath.Join(common.Stage1RootfsPath(p.Root), "flavor"))
	if err != nil {
		return "", -1, errwrap.Wrap(errors.New("unable to determine stage1 flavor"), err)
	}

	if flavor == "host" {
		// This flavor does not contain systemd, parse "systemctl --version"
		systemctlBin, err := common.LookupPath("systemctl", os.Getenv("PATH"))
		if err != nil {
			return "", -1, err
		}

		systemdVersion, err := common.SystemdVersion(systemctlBin)
		if err != nil {
			return "", -1, errwrap.Wrap(errors.New("error finding systemctl version"), err)
		}

		return flavor, systemdVersion, nil
	}

	systemdVersionBytes, err := ioutil.ReadFile(filepath.Join(common.Stage1RootfsPath(p.Root), "systemd-version"))
	if err != nil {
		return "", -1, errwrap.Wrap(errors.New("unable to determine stage1's systemd version"), err)
	}
	systemdVersionString := strings.Trim(string(systemdVersionBytes), " \n")

	// systemdVersionString is either a tag name or a branch name. If it's a
	// tag name it's of the form "v229", remove the first character to get the
	// number.
	systemdVersion, err = strconv.Atoi(systemdVersionString[1:])
	if err != nil {
		// If we get a syntax error, it means the parsing of the version string
		// of the form "v229" failed, set it to 0 to indicate we couldn't guess
		// it.
		if e, ok := err.(*strconv.NumError); ok && e.Err == strconv.ErrSyntax {
			systemdVersion = 0
		} else {
			return "", -1, errwrap.Wrap(errors.New("error parsing stage1's systemd version"), err)
		}
	}
	return flavor, systemdVersion, nil
}

// GetAppHashes returns a list of hashes of the apps in this pod
func GetAppHashes(p *stage1commontypes.Pod) []types.Hash {
	var names []types.Hash
	for _, a := range p.Manifest.Apps {
		names = append(names, a.Image.ID)
	}

	return names
}

// GetMachineID returns the machine id string of the pod to be passed to
// systemd-nspawn
func GetMachineID(p *stage1commontypes.Pod) string {
	return "rkt-" + p.UUID.String()
}

// getAppCapabilities computes the set of Linux capabilities that an app
// should have based on its isolators. Only the following capabalities matter:
// - os/linux/capabilities-retain-set
// - os/linux/capabilities-remove-set
//
// The resulting capabilities are generated following the rules from the spec:
// See: https://github.com/appc/spec/blob/master/spec/ace.md#linux-isolators
func getAppCapabilities(isolators types.Isolators) ([]string, error) {
	var capsToRetain []string
	var capsToRemove []string

	// Default caps defined in
	// https://github.com/appc/spec/blob/master/spec/ace.md#linux-isolators
	appDefaultCapabilities := []string{
		"CAP_AUDIT_WRITE",
		"CAP_CHOWN",
		"CAP_DAC_OVERRIDE",
		"CAP_FSETID",
		"CAP_FOWNER",
		"CAP_KILL",
		"CAP_MKNOD",
		"CAP_NET_RAW",
		"CAP_NET_BIND_SERVICE",
		"CAP_SETUID",
		"CAP_SETGID",
		"CAP_SETPCAP",
		"CAP_SETFCAP",
		"CAP_SYS_CHROOT",
	}

	// Iterate over the isolators defined in
	// https://github.com/appc/spec/blob/master/spec/ace.md#linux-isolators
	// Only read the capababilities isolators:
	// - os/linux/capabilities-retain-set
	// - os/linux/capabilities-remove-set
	for _, isolator := range isolators {
		if capSet, ok := isolator.Value().(types.LinuxCapabilitiesSet); ok {
			switch isolator.Name {
			case types.LinuxCapabilitiesRetainSetName:
				capsToRetain = append(capsToRetain, parseLinuxCapabilitiesSet(capSet)...)
			case types.LinuxCapabilitiesRevokeSetName:
				capsToRemove = append(capsToRemove, parseLinuxCapabilitiesSet(capSet)...)
			}
		}
	}

	// appc/spec does not allow to have both the retain set and the remove
	// set defined.
	if len(capsToRetain) > 0 && len(capsToRemove) > 0 {
		return nil, errors.New("cannot have both os/linux/capabilities-retain-set and os/linux/capabilities-remove-set")
	}

	// Neither the retain set or the remove set are defined
	if len(capsToRetain) == 0 && len(capsToRemove) == 0 {
		return appDefaultCapabilities, nil
	}

	if len(capsToRetain) > 0 {
		return capsToRetain, nil
	}

	if len(capsToRemove) == 0 {
		panic("len(capsToRetain) is negative. This cannot happen.")
	}

	caps := appDefaultCapabilities
	for _, rc := range capsToRemove {
		// backward loop to be safe against deletion
		for i := len(caps) - 1; i >= 0; i-- {
			if caps[i] == rc {
				caps = append(caps[:i], caps[i+1:]...)
			}
		}
	}
	return caps, nil
}

// parseLinuxCapabilitySet parses a LinuxCapabilitiesSet into string slice
func parseLinuxCapabilitiesSet(capSet types.LinuxCapabilitiesSet) []string {
	var capsStr []string
	for _, cap := range capSet.Set() {
		capsStr = append(capsStr, string(cap))
	}
	return capsStr
}

func getAppNoNewPrivileges(isolators types.Isolators) bool {
	for _, isolator := range isolators {
		noNewPrivileges, ok := isolator.Value().(*types.LinuxNoNewPrivileges)

		if ok && bool(*noNewPrivileges) {
			return true
		}
	}

	return false
}

// restrictProcFS restricts access to some security-sensitive paths under
// /proc and /sys. Entries are either hidden or just made read-only to app.
func protectSystemFiles(opts []*unit.UnitOption, appName types.ACName) []*unit.UnitOption {
	roPaths := []string{
		"/proc/bus/",
		"/proc/sys/kernel/core_pattern",
		"/proc/sys/kernel/modprobe",
		"/proc/sys/vm/panic_on_oom",
		"/proc/sysrq-trigger",
		"/sys/block/",
		"/sys/bus/",
		"/sys/class/",
		"/sys/dev/",
		"/sys/devices/",
		"/sys/kernel/",
	}
	hiddenPaths := []string{
		// TODO(lucab): file-paths restrictions need support in systemd first
		//"/proc/config.gz",
		//"/proc/kallsyms",
		//"/proc/sched_debug",
		//"/proc/kcore",
		//"/proc/kmem",
		//"/proc/mem",
		"/sys/firmware/",
		"/sys/fs/",
		"/sys/hypervisor/",
		"/sys/module/",
		"/sys/power/",
	}
	// Paths prefixed with "-" are ignored if they do not exist:
	// https://www.freedesktop.org/software/systemd/man/systemd.exec.html#ReadWriteDirectories=
	for _, p := range hiddenPaths {
		opts = append(opts, unit.NewUnitOption("Service", "InaccessibleDirectories", fmt.Sprintf("-%s", filepath.Join(common.RelAppRootfsPath(appName), p))))
	}
	for _, p := range roPaths {
		opts = append(opts, unit.NewUnitOption("Service", "ReadOnlyDirectories", fmt.Sprintf("-%s", filepath.Join(common.RelAppRootfsPath(appName), p))))
	}
	return opts
}
