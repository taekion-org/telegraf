package systemd_units

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/plugins/inputs"
)

// SystemdUnits is a telegraf plugin to gather systemd unit status
type SystemdUnits struct {
	Timeout   internal.Duration
	UnitType  string `toml:"unittype"`
	systemctl systemctl
}

type SystemdData struct {
	name   string
	state  string
	load   string
	active string
	sub    string
	fields map[string]interface{}
}

type systemctl func(Timeout internal.Duration, UnitType string, InterfaceType string) (*bytes.Buffer, error)

const measurement = "systemd_units"

// Below are mappings of systemd state tables as defined in
// https://github.com/systemd/systemd/blob/c87700a1335f489be31cd3549927da68b5638819/src/basic/unit-def.c
// Duplicate strings are removed from this list.
var load_map = map[string]int{
	"loaded":      0,
	"stub":        1,
	"not-found":   2,
	"bad-setting": 3,
	"error":       4,
	"merged":      5,
	"masked":      6,
	"null":        10,
}

var active_map = map[string]int{
	"active":       0,
	"reloading":    1,
	"inactive":     2,
	"failed":       3,
	"activating":   4,
	"deactivating": 5,
	"null":         10,
}

var files_map = map[string]int{
	"disabled":        0,
	"enabled":         1,
	"enabled-runtime": 2,
	"generated":       3,
	"indirect":        4,
	"masked":          5,
	"static":          6,
	"transient":       7,
	"null":            10,
}

var sub_map = map[string]int{
	// service_state_table, offset 0x0000
	"running":       0x0000,
	"dead":          0x0001,
	"start-pre":     0x0002,
	"start":         0x0003,
	"exited":        0x0004,
	"reload":        0x0005,
	"stop":          0x0006,
	"stop-watchdog": 0x0007,
	"stop-sigterm":  0x0008,
	"stop-sigkill":  0x0009,
	"stop-post":     0x000a,
	"final-sigterm": 0x000b,
	"failed":        0x000c,
	"auto-restart":  0x000d,

	// automount_state_table, offset 0x0010
	"waiting": 0x0010,

	// device_state_table, offset 0x0020
	"tentative": 0x0020,
	"plugged":   0x0021,

	// mount_state_table, offset 0x0030
	"mounting":           0x0030,
	"mounting-done":      0x0031,
	"mounted":            0x0032,
	"remounting":         0x0033,
	"unmounting":         0x0034,
	"remounting-sigterm": 0x0035,
	"remounting-sigkill": 0x0036,
	"unmounting-sigterm": 0x0037,
	"unmounting-sigkill": 0x0038,

	// path_state_table, offset 0x0040

	// scope_state_table, offset 0x0050
	"abandoned": 0x0050,

	// slice_state_table, offset 0x0060
	"active": 0x0060,

	// socket_state_table, offset 0x0070
	"start-chown":      0x0070,
	"start-post":       0x0071,
	"listening":        0x0072,
	"stop-pre":         0x0073,
	"stop-pre-sigterm": 0x0074,
	"stop-pre-sigkill": 0x0075,
	"final-sigkill":    0x0076,

	// swap_state_table, offset 0x0080
	"activating":           0x0080,
	"activating-done":      0x0081,
	"deactivating":         0x0082,
	"deactivating-sigterm": 0x0083,
	"deactivating-sigkill": 0x0084,

	// target_state_table, offset 0x0090

	// timer_state_table, offset 0x00a0
	"elapsed": 0x00a0,
	"null":    0x00ff,
}

var (
	defaultTimeout  = internal.Duration{Duration: time.Second}
	defaultUnitType = "service"
)

// Description returns a short description of the plugin
func (s *SystemdUnits) Description() string {
	return "Gather systemd units state"
}

// SampleConfig returns sample configuration options.
func (s *SystemdUnits) SampleConfig() string {
	return `
  ## Set timeout for systemctl execution
  # timeout = "1s"
  #
  ## Filter for a specific unit type, default is "service", other possible
  ## values are "socket", "target", "device", "mount", "automount", "swap",
  ## "timer", "path", "slice" and "scope ":
  # unittype = "service"
`
}

// Gather parses systemctl outputs and adds counters to the Accumulator
func (s *SystemdUnits) Gather(acc telegraf.Accumulator) error {
	out, err := s.systemctl(s.Timeout, s.UnitType, "list-units")
	if err != nil {
		return err
	}

	var out2 *bytes.Buffer
	out2, err = s.systemctl(s.Timeout, s.UnitType, "list-unit-files")
	if err != nil {
		return err
	}

	tags := make(map[string]*SystemdData)
	scanner2 := bufio.NewScanner(out2)
	for scanner2.Scan() {
		line := scanner2.Text()

		data := strings.Fields(line)
		if len(data) < 2 {
			acc.AddError(fmt.Errorf("Error parsing line (expected at least 2 fields): %s", line))
			continue
		}

		name := data[0]
		state := data[1]
		tags[name] = &SystemdData{
			name:   name,
			state:  state,
			load:   "null",
			active: "null",
			sub:    "null",
		}
	}

	scanner := bufio.NewScanner(out)
	for scanner.Scan() {
		line := scanner.Text()

		data := strings.Fields(line)
		if len(data) < 4 {
			acc.AddError(fmt.Errorf("Error parsing line (expected at least 4 fields): %s", line))
			continue
		}

		name := data[0]
		load := data[1]
		active := data[2]
		sub := data[3]

		if load == "" {
			load = "null"
		}
		if active == "" {
			active = "null"
		}
		if sub == "" {
			sub = "null"
		}

		var (
			loadCode   int
			activeCode int
			subCode    int
			filesCode  int
			ok         bool
		)

		if loadCode, ok = load_map[load]; !ok {
			acc.AddError(fmt.Errorf("Error parsing field 'load', value not in map: %s", load))
			continue
		}
		if activeCode, ok = active_map[active]; !ok {
			acc.AddError(fmt.Errorf("Error parsing field 'active', value not in map: %s", active))
			continue
		}
		if subCode, ok = sub_map[sub]; !ok {
			acc.AddError(fmt.Errorf("Error parsing field 'sub', value not in map: %s", sub))
			continue
		}
		if _, ok = tags[name]; ok {
			if filesCode, ok = files_map[tags[name].state]; !ok {
				acc.AddError(fmt.Errorf("Error parsing field 'state', %s value not in map: %s", name))
				continue
			}
		}

		fields := map[string]interface{}{
			"load_code":   loadCode,
			"active_code": activeCode,
			"sub_code":    subCode,
			"state_code":  filesCode,
		}

		if _, ok = tags[name]; !ok || tags[name].state == "" {
			tags[name] = &SystemdData{
				name:   name,
				state:  "null",
				load:   load,
				active: active,
				sub:    sub,
				fields: fields,
			}
		} else {
			tags[name] = &SystemdData{
				name:   name,
				state:  tags[name].state,
				load:   load,
				active: active,
				sub:    sub,
				fields: fields,
			}
		}

		for _, data := range tags {
			acc.AddFields(measurement, data.fields, map[string]string{"name": data.name, "state": data.state, "load": data.load, "sub": data.sub}, time.Now())
		}

	}
	return nil
}

func setSystemctl(Timeout internal.Duration, UnitType string, InterfaceType string) (*bytes.Buffer, error) {
	// is systemctl available ?
	systemctlPath, err := exec.LookPath("systemctl")
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(systemctlPath, InterfaceType, "--all", fmt.Sprintf("--type=%s", UnitType), "--no-legend")

	var out bytes.Buffer
	cmd.Stdout = &out
	err = internal.RunTimeout(cmd, Timeout.Duration)
	if err != nil {
		return &out, fmt.Errorf("error running systemctl %s --all --type=%s --no-legend: %s", InterfaceType, UnitType, err)
	}

	return &out, nil
}

func init() {
	inputs.Add("systemd_units", func() telegraf.Input {
		return &SystemdUnits{
			systemctl: setSystemctl,
			Timeout:   defaultTimeout,
			UnitType:  defaultUnitType,
		}
	})
}
