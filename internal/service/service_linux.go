// SPDX-FileCopyrightText: 2026 ESP-KVM contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	exePath      = "/usr/local/bin/espkvm-tools"
	unitPath     = "/etc/systemd/system/espkvm-tools.service"
	userUnit     = "/etc/systemd/user/espkvm-tools.service"
	udevPath     = "/etc/udev/rules.d/70-espkvm-tools.rules"
	sysusersPath = "/etc/sysusers.d/espkvm-tools.conf"
	// polkit reads one or the other, depending on its version.
	polkitRules = "/etc/polkit-1/rules.d/50-espkvm-tools.rules"
	polkitPkla  = "/etc/polkit-1/localauthority/50-local.d/50-espkvm-tools.pkla"
)

// The system service runs as its own user, not root.
const sysUser = "espkvm-tools"

const sysusers = `# ESP-KVM Tools (installed by espkvm-tools)
u espkvm-tools - "ESP-KVM Tools"
`

// ModemManager keeps its hands off the port. The system service gets it through
// its group. In desktop mode the user at the screen may open it (uaccess).
// 4033 is the same port with the USB speaker on too.
const udevSystem = `# ESP-KVM Tools port (installed by espkvm-tools)
SUBSYSTEM=="tty", ATTRS{idVendor}=="303a", ATTRS{idProduct}=="4013", ENV{ID_MM_DEVICE_IGNORE}="1", GROUP="espkvm-tools", MODE="0660", SYMLINK+="espkvm-tools"
SUBSYSTEM=="tty", ATTRS{idVendor}=="303a", ATTRS{idProduct}=="4033", ENV{ID_MM_DEVICE_IGNORE}="1", GROUP="espkvm-tools", MODE="0660", SYMLINK+="espkvm-tools"
`

const udevDesktop = `# ESP-KVM Tools port (installed by espkvm-tools)
SUBSYSTEM=="tty", ATTRS{idVendor}=="303a", ATTRS{idProduct}=="4013", ENV{ID_MM_DEVICE_IGNORE}="1", TAG+="uaccess", SYMLINK+="espkvm-tools"
SUBSYSTEM=="tty", ATTRS{idVendor}=="303a", ATTRS{idProduct}=="4033", ENV{ID_MM_DEVICE_IGNORE}="1", TAG+="uaccess", SYMLINK+="espkvm-tools"
`

// It reads the port, /etc/os-release, /proc and the interfaces, and may run
// systemctl. Everything else is shut.
const systemUnit = `[Unit]
Description=ESP-KVM Tools
After=network.target

[Service]
ExecStart=%s run%s
Restart=always
RestartSec=3
User=espkvm-tools
Group=espkvm-tools
NoNewPrivileges=yes
CapabilityBoundingSet=
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictAddressFamilies=AF_UNIX AF_NETLINK
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
`

// In the graphical session, so WAYLAND_DISPLAY or DISPLAY is there.
const desktopUnit = `[Unit]
Description=ESP-KVM Tools
PartOf=graphical-session.target
After=graphical-session.target

[Service]
ExecStart=%s run%s
Restart=always
RestartSec=3

[Install]
WantedBy=graphical-session.target
`

// systemctl poweroff/reboot from a user that is not root and has no session.
var powerActions = []string{
	"org.freedesktop.login1.power-off",
	"org.freedesktop.login1.power-off-multiple-sessions",
	"org.freedesktop.login1.reboot",
	"org.freedesktop.login1.reboot-multiple-sessions",
}

const polkitJS = `// ESP-KVM Tools (installed by espkvm-tools --allow-power)
polkit.addRule(function(action, subject) {
    if (subject.user == "espkvm-tools" && [%s].indexOf(action.id) >= 0)
        return polkit.Result.YES;
});
`

const pkla = `[ESP-KVM Tools power]
Identity=unix-user:espkvm-tools
Action=%s
ResultAny=yes
ResultInactive=yes
ResultActive=yes
`

func Install(o Options) error {
	if os.Geteuid() != 0 {
		return errors.New("run it with sudo: it writes a udev rule and a systemd unit")
	}
	removeOld()
	if err := copyExe(exePath); err != nil {
		return err
	}
	args := ""
	if len(o.Args) > 0 {
		args = " " + strings.Join(o.Args, " ")
	}
	if o.Desktop {
		if err := writeUdev(udevDesktop); err != nil {
			return err
		}
		if err := os.WriteFile(userUnit, []byte(fmt.Sprintf(desktopUnit, exePath, args)), 0o644); err != nil {
			return err
		}
		if err := sh("systemctl", "--global", "enable", "espkvm-tools.service"); err != nil {
			return err
		}
		fmt.Println("Installed for every user's desktop session. It starts at the next login,")
		fmt.Println("or now with: systemctl --user start espkvm-tools")
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(sysusersPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(sysusersPath, []byte(sysusers), 0o644); err != nil {
		return err
	}
	if err := sh("systemd-sysusers", sysusersPath); err != nil {
		return err
	}
	if err := writeUdev(udevSystem); err != nil {
		return err
	}
	removePolkit()
	if o.Power {
		if err := writePolkit(); err != nil {
			return err
		}
	}
	if err := os.WriteFile(unitPath, []byte(fmt.Sprintf(systemUnit, exePath, args)), 0o644); err != nil {
		return err
	}
	_ = sh("systemctl", "daemon-reload")
	if err := sh("systemctl", "enable", "espkvm-tools.service"); err != nil {
		return err
	}
	// restart, not start: an older copy may be running as root.
	if err := sh("systemctl", "restart", "espkvm-tools.service"); err != nil {
		return err
	}
	fmt.Println("Installed and started as user " + sysUser + ": systemctl status espkvm-tools")
	return nil
}

func writeUdev(rule string) error {
	if err := os.WriteFile(udevPath, []byte(rule), 0o644); err != nil {
		return err
	}
	_ = sh("udevadm", "control", "--reload")
	_ = sh("udevadm", "trigger", "--subsystem-match=tty", "--attr-match=idVendor=303a")
	_ = sh("udevadm", "settle")
	return nil
}

// Both files where polkit has the directory: which one it reads depends on
// its version (0.105 the .pkla, newer the .rules).
func writePolkit() error {
	quoted := make([]string, len(powerActions))
	for i, a := range powerActions {
		quoted[i] = `"` + a + `"`
	}
	wrote := false
	if dirExists(filepath.Dir(polkitRules)) {
		if err := os.WriteFile(polkitRules, []byte(fmt.Sprintf(polkitJS, strings.Join(quoted, ", "))), 0o644); err != nil {
			return err
		}
		wrote = true
	}
	if dirExists(filepath.Dir(filepath.Dir(polkitPkla))) {
		if err := os.MkdirAll(filepath.Dir(polkitPkla), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(polkitPkla, []byte(fmt.Sprintf(pkla, strings.Join(powerActions, ";"))), 0o644); err != nil {
			return err
		}
		wrote = true
	}
	if !wrote {
		return errors.New("--allow-power: no polkit found, so the service user cannot shut the machine down")
	}
	return nil
}

func removePolkit() {
	_ = os.Remove(polkitRules)
	_ = os.Remove(polkitPkla)
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func Uninstall() error {
	if os.Geteuid() != 0 {
		return errors.New("run it with sudo")
	}
	_ = sh("systemctl", "disable", "--now", "espkvm-tools.service")
	_ = sh("systemctl", "--global", "disable", "espkvm-tools.service")
	removePolkit()
	// The user stays: removing users is left to the admin, as sysusers does.
	for _, p := range []string{unitPath, userUnit, udevPath, sysusersPath, exePath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	_ = sh("systemctl", "daemon-reload")
	_ = sh("udevadm", "control", "--reload")
	fmt.Println("Removed. A desktop copy still running stops at the next logout.")
	return nil
}

// Before the rename the program was espkvm-agent. Its service, rules and
// binary go, so the two never fight over the port. Its user stays, as in
// Uninstall.
func removeOld() {
	const old = "espkvm-agent"
	_ = exec.Command("systemctl", "disable", "--now", old+".service").Run()
	_ = exec.Command("systemctl", "--global", "disable", old+".service").Run()
	for _, p := range []string{
		"/etc/systemd/system/" + old + ".service",
		"/etc/systemd/user/" + old + ".service",
		"/etc/udev/rules.d/70-" + old + ".rules",
		"/etc/sysusers.d/" + old + ".conf",
		"/etc/polkit-1/rules.d/50-" + old + ".rules",
		"/etc/polkit-1/localauthority/50-local.d/50-" + old + ".pkla",
		"/usr/local/bin/" + old,
	} {
		if os.Remove(p) == nil {
			fmt.Println("Removed the old " + p)
		}
	}
}

func sh(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}
