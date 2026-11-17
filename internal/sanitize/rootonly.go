package sanitize

import (
	"slices"
	"strings"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
)

// RootOnly lists what in a backed-up config only root@pam could restore
// or remove from the copy, so PBP's token can never proof the guest
// (issue #140). It follows PVE's own permission checks (pve-container's
// check_ct_modify_config_perm and restore merge, qemu-server's
// check_restore_permissions and config update checks):
//
//   - raw: the sorted keys of a container's raw lxc.* lines that are not
//     device-only (PVE drops raw lines unless root@pam restores);
//   - keys: a container's devN not on the keep-list and its hookscript; a
//     VM's hookscript, ivshmem, parallelN, a serialN that is not a socket,
//     a usbN on a real host device, and a hostpciN on a raw host device or
//     with a romfile. Detail names the device where the value has one.
//
// Mapped VM devices (usbN, hostpciN without romfile, virtiofsN, rngN on
// /dev/hwrng) need Mapping.Use, a grant rather than root@pam, and are not
// listed. Keys come hookscript and ivshmem first, then by name and number.
func RootOnly(kind model.GuestKind, orig map[string]string) (raw []string, keys []KeyDetail) {
	for _, k := range []string{"hookscript", "ivshmem"} {
		if _, ok := orig[k]; ok && (k == "hookscript" || kind == model.KindQEMU) {
			keys = append(keys, KeyDetail{Key: k})
		}
	}
	if kind == model.KindLXC {
		for _, l := range RawLXCLines(orig) {
			if !DeviceOnly(l) && !slices.Contains(raw, l.Key) {
				raw = append(raw, l.Key)
			}
		}
		slices.Sort(raw)
		for _, k := range indexedKeys(orig, reDev) {
			if p := DevicePath(orig[k]); !KeptDevice(p) {
				keys = append(keys, KeyDetail{Key: k, Detail: p})
			}
		}
		return raw, keys
	}
	for _, k := range indexedKeys(orig, rePCIEtc) {
		if detail, blocks := rootOnlyDevice(k, orig[k]); blocks {
			keys = append(keys, KeyDetail{Key: k, Detail: detail})
		}
	}
	return nil, keys
}

// rootOnlyDevice decides whether a VM's hostpciN, usbN, serialN or
// parallelN is root@pam's alone, and what device it names.
func rootOnlyDevice(k, v string) (detail string, blocks bool) {
	first := strings.TrimSpace(firstField(v))
	bare := first
	if strings.Contains(first, "=") {
		bare = ""
	}
	host, hasHost := optValue(v, "host")
	if !hasHost {
		host = bare
	}
	switch {
	case strings.HasPrefix(k, "parallel"):
		return first, true
	case strings.HasPrefix(k, "serial"):
		return first, first != "socket"
	case strings.HasPrefix(k, "usb"):
		return host, host != "" && !strings.EqualFold(host, "spice")
	default: // hostpci
		if host != "" {
			return host, true
		}
		if _, ok := optValue(v, "romfile"); ok {
			return "romfile", true
		}
		return "", false
	}
}
