package ceph

import "testing"

func TestParseLsblkJSON(t *testing.T) {
	raw := []byte(`{
		"blockdevices": [
			{"name": "sda", "serial": "ABC123", "wwn": "0x5000C500A1B2C3D4"},
			{"name": "nvme0n1", "serial": "", "wwn": ""}
		]
	}`)

	mapping := ParseLsblkJSON(raw)
	if len(mapping) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(mapping))
	}
	if mapping["sda"].Serial != "ABC123" {
		t.Errorf("expected serial ABC123, got %q", mapping["sda"].Serial)
	}
	if mapping["sda"].WWN != "5000c500a1b2c3d4" {
		t.Errorf("expected normalized wwn, got %q", mapping["sda"].WWN)
	}
}

func TestParseSmartpqiEvents(t *testing.T) {
	raw := `
Jul 21 10:00:00 host kernel: smartpqi 0000:03:00.0: removed 1:0:5:0 500A1B2C3D4E5F60112233445566778899 sda
Jul 21 10:00:05 host kernel: smartpqi 0000:03:00.0: added 1:0:5:0 500A1B2C3D4E5F60112233445566778899 sdc
`
	events := ParseSmartpqiEvents(raw)
	ev, ok := events["500a1b2c3d4e5f60112233445566778899"]
	if !ok {
		t.Fatalf("expected event for normalized wwn, got %v", events)
	}
	if len(ev.Removed) != 1 || ev.Removed[0] != "sda" {
		t.Errorf("expected removed=[sda], got %v", ev.Removed)
	}
	if len(ev.Added) != 1 || ev.Added[0] != "sdc" {
		t.Errorf("expected added=[sdc], got %v", ev.Added)
	}
}

func TestParseCephDeviceList(t *testing.T) {
	raw := `DEVICE                        DEV      DAEMONS
HPE_MO001918RWFRK_SERIAL0001  sda      osd.5
HPE_MO001918RWFRK_SERIAL0002  sdb      osd.12 osd.13
UNRELATED_LINE_NO_OSD         sdz      mon.a
`
	deviceMap := ParseCephDeviceList(raw)

	if len(deviceMap) != 2 {
		t.Fatalf("expected 2 devices with OSDs, got %d: %v", len(deviceMap), deviceMap)
	}
	if deviceMap["sda"].Serial != "SERIAL0001" {
		t.Errorf("expected serial SERIAL0001, got %q", deviceMap["sda"].Serial)
	}
	if len(deviceMap["sda"].OSDIDs) != 1 || deviceMap["sda"].OSDIDs[0] != "5" {
		t.Errorf("expected osd_ids=[5], got %v", deviceMap["sda"].OSDIDs)
	}
	if len(deviceMap["sdb"].OSDIDs) != 2 {
		t.Errorf("expected 2 osd_ids for sdb, got %v", deviceMap["sdb"].OSDIDs)
	}
	if _, ok := deviceMap["sdz"]; ok {
		t.Errorf("device with no osd.N daemon should be excluded, got %v", deviceMap["sdz"])
	}
}

func TestBaseDeviceName(t *testing.T) {
	tests := map[string]string{
		"sda1":      "sda",
		"sda":       "sda",
		"nvme0n1p2": "nvme0n1",
		"nvme0n1":   "nvme0n1",
	}
	for in, want := range tests {
		if got := BaseDeviceName(in); got != want {
			t.Errorf("BaseDeviceName(%q) = %q, want %q", in, got, want)
		}
	}
}
