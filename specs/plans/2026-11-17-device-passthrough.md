# Containers with devices Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Refuse guests whose copy needs a `root@pam`-only change before restoring them (#140), keep purely virtual container devices in the copy, and proof containers whose raw `lxc.*` lines only pass devices through (#141).

**Architecture:** Pure classification lives in `internal/sanitize`:
- the keep-list;
- the character-device list;
- device-only raw lines;
- the `root@pam`-only keys.

The runner calls it in place of today's raw `lxc.*` check, before anything is reserved. Dropped device lines become `passthrough` missing pieces through `MissingFromOriginal`. The Linux probe traces failed units to them through each unit's journal, and adds an `ignore_units` hint when it cannot.

**Tech Stack:** Go 1.26, the repo's own fakes (`internal/pve/pvefake`, the run fixture, the probe's scripted executor).

**Spec:** `specs/2026-11-17-device-passthrough-design.md`

## Global Constraints

- Tests run on the owner's development host, not the workstation: sync the working tree there (without `.git`, `.superpowers`, `.remember`, `dist`) and run `go test` in it. "Run:" lines below give only the `go test` part.
- Before each commit: `gofmt -l` clean, and `go vet ./...` and `golangci-lint run` (on the development host) clean.
- **Commits:** signed with the owner's SSH signing key through their ssh-agent, and each message ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- **No lab details in repo files** (host names, guest names or IDs, storage names, IPs). Use placeholders such as `web01.example.lan`.
- The docs anchor for every refusal is `docsToken` (`docs/security.md#guests-pbp-cannot-proof`).
- Reasons must fit the notes line. The runner caps them as today; name at most three keys, then `and N more` (`keyList` style).
- **Keep-list, exact paths:** `/dev/net/tun`, `/dev/fuse`, `/dev/null`, `/dev/zero`, `/dev/full`, `/dev/random`, `/dev/urandom`.
- **Character-device list** (an entry ending `/` covers paths under it; an entry ending `*` is a name prefix; others are exact):
  - `/dev/tty*`, `/dev/serial/`, `/dev/dri/`, `/dev/bus/usb/`, `/dev/usb/`;
  - `/dev/net/tun`, `/dev/fuse`, `/dev/apex_*`, `/dev/accel/`, `/dev/video*`, `/dev/snd/`;
  - `/dev/hidraw*`, `/dev/input/`, `/dev/nvidia*`, `/dev/kfd`, `/dev/ppp`, `/dev/i2c-*`, `/dev/gpiochip*`.
- Sanitize keeps removing every key it removes today, except keep-listed `devN`. It is the safety net.

## Review Focus

1. **Spacing and options in `lxc.mount.entry`.** Fields are separated by runs of spaces or tabs, as in `/dev/ttyUSB0       dev/ttyUSB0       none bind,optional,create=file`. Options may come in any order, e.g. `rbind,create=dir`. Parse with `strings.Fields`; Task 1 tests the real spacing.
2. **`devN` values in `path=` form.** PVE accepts `dev0: path=/dev/net/tun,mode=0666` as well as `dev0: /dev/net/tun,uid=0`. Both must yield the path for the keep-list and the refusal; Task 1 tests both.
3. **Device paths in the journal followed by punctuation**, e.g. `open("/dev/ttyUSB0"): No such file or directory` or `/dev/ttyACM0: …`. The path must be found without its quote or colon; Task 5 tests it.
4. **Mixed raw lines.** A container with device-only lines *and* one non-device line ends `ERROR`, naming only the non-device key(s), and is never restored. Task 2 tests the classifier; Task 3 tests the run.
5. **A mapped `hostpciN` without `romfile`, a mapped `usbN`, and `usbN: spice`** must not be refused: they need `Mapping.Use` or nothing, not `root@pam`. Task 2 tests them.

---

### Task 1: Device rules in sanitize (keep-list, character devices, device-only raw lines)

**Files:**
- Create: `internal/sanitize/devices.go`, `internal/sanitize/devices_test.go`
- Modify: `internal/sanitize/sanitize.go` (the `reDev` case), `internal/sanitize/original.go`, `internal/sanitize/sanitize_test.go`, `internal/sanitize/original_test.go`

**Interfaces:**
- Produces:
  - `type RawLine struct{ Key, Value string }`
  - `func RawLXCLines(cfg map[string]string) []RawLine`: from `cfg["lxc"]` as `ParseConfigText` gathers it, in config order, key and value trimmed.
  - `func DeviceOnly(l RawLine) bool`
  - `func DevicePath(v string) string`: the path of a `devN` value, bare or `path=` form, before any options.
  - `func KeptDevice(path string) bool`: exact keep-list match.
  - `type KeyDetail struct{ Key, Detail string }`
  - `func KeptDevices(cfg map[string]string) []KeyDetail`: keep-listed `devN`, in numeric key order (`dev2` before `dev10`), with `Detail` the path.
  - `MissingFromOriginal(model.KindLXC, orig)` additionally returns the pieces of dropped device lines (see Step 3), **only when every raw line is device-only**.

- [ ] **Step 1: Write the failing tests** in `devices_test.go`:

```go
func TestDeviceOnly(t *testing.T) {
	yes := []string{
		"lxc.cgroup2.devices.allow: a",
		"lxc.cgroup2.devices.allow: c 188:* rwm",
		"lxc.cgroup.devices.allow: b 8:* rwm",
		"lxc.mount.entry: /dev/serial/by-id  dev/serial/by-id  none bind,optional,create=dir",
		"lxc.mount.entry: /dev/ttyUSB0       dev/ttyUSB0       none bind,optional,create=file",
		"lxc.mount.entry: /dev/net/tun dev/net/tun none bind,create=file",
		"lxc.mount.entry: /dev/dri dev/dri none rbind,optional,create=dir",
		"lxc.mount.entry: /dev/bus/usb/001 dev/bus/usb/001 none bind,optional,create=dir",
	}
	no := []string{
		"lxc.cgroup2.devices.deny: c 10:200 rwm",
		"lxc.mount.entry: /dev/sdb1 mnt/data ext4 defaults",
		"lxc.mount.entry: /dev/sdb1 mnt/data none bind,create=file",
		"lxc.mount.entry: /dev/disk/by-id/x mnt/x none bind",
		"lxc.mount.entry: /dev/zvol/rpool/x mnt/x none bind",
		"lxc.mount.entry: /mnt/media mnt/media none bind",
		"lxc.mount.entry: /dev/ttyUSB0 dev/ttyUSB0 none ro", // no bind
		"lxc.mount.entry: /dev/ttyUSB0",                     // unparseable
		"lxc.autodev: 1",
		"lxc.hook.autodev: sh -c 'mknod …'",
		"lxc.apparmor.profile: unconfined",
		"lxc.idmap: u 0 100000 65536",
		"lxc.cap.drop:",
	}
	// each yes → true, each no → false (parse with RawLXCLines(ParseConfigText(line+"\n"))[0])
}

func TestDevicePathAndKeepList(t *testing.T) {
	// DevicePath: "/dev/net/tun" → "/dev/net/tun"; "/dev/net/tun,uid=0,mode=0666" → "/dev/net/tun";
	// "path=/dev/net/tun,mode=0666" → "/dev/net/tun"; "mode=0666,path=/dev/fuse" → "/dev/fuse".
	// KeptDevice true: every keep-list entry. false: "/dev/net/tun2", "/dev/net", "/dev/kvm",
	// "/dev/loop0", "/dev/loop-control", "/dev/ttyUSB0", "/dev/dri/renderD128", "/dev/net/../net/tun".
	// KeptDevices({"dev10": "/dev/fuse", "dev2": "path=/dev/net/tun", "dev0": "/dev/ttyUSB0"})
	//   == []KeyDetail{{"dev2", "/dev/net/tun"}, {"dev10", "/dev/fuse"}}
}
```

In `sanitize_test.go`, change `TestSanitizeLXCMounts`, or add a test, so that:
- `dev0: /dev/net/tun,mode=0666` is **not** in `Delete` or `Removed`;
- `dev1: /dev/dri/card0` still is (`passthrough`, `dev1`).

In `original_test.go`, add `TestMissingFromOriginalDroppedDevices`. Input: the real set from #141 (three allow lines, `c 10:200 rwm`, and binds of `/dev/serial/by-id`, `ttyUSB0`, `ttyUSB1`, `ttyACM0`, `ttyACM1`, `/dev/net/tun`), plus one line duplicated. Expected pieces, in config order, duplicates collapsed:
- `{Kind: "passthrough", Key: "lxc.cgroup2.devices.allow", Detail: "a"}` (and one per distinct allow rule);
- `{Kind: "passthrough", Key: "lxc.mount.entry", Path: "/dev/ttyUSB0"}`;
- a source different from its target, e.g. `/dev/serial/by-id/usb-x dev/ttyACM0 none bind`, gives `Path: "/dev/ttyACM0", Detail: "/dev/serial/by-id/usb-x"`.

The same input plus `lxc.apparmor.profile: unconfined` returns **no** device pieces.

- [ ] **Step 2: Run them, expect FAIL** (undefined `RawLXCLines`, `DeviceOnly`, …)

Run: `go test ./internal/sanitize/`
Expected: build failure naming the undefined functions.

- [ ] **Step 3: Implement** `devices.go`, and change `Sanitize`'s `reDev` case to `if !KeptDevice(DevicePath(v)) { p.remove(…) }`.

**`DeviceOnly`:**
- `lxc.cgroup2.devices.allow` / `lxc.cgroup.devices.allow`: always true.
- `lxc.mount.entry`: true only when all of these hold:
  - `strings.Fields(value)` has at least 4 fields;
  - field 3 is `none`;
  - field 4, split on `,`, contains `bind` or `rbind`;
  - field 1, after `path.Clean`, is on the character-device list.
- Everything else: false.

**`MissingFromOriginal`, LXC:** when `RawLXCLines` is non-empty and all are `DeviceOnly`, append one piece per distinct line:
- **Allow line:** `Key` the raw key, `Detail` the value.
- **Mount entry:** `Path` = `"/" + strings.TrimPrefix(path.Clean(target), "/")`; `Detail` = the cleaned source when it differs from `Path`.

- [ ] **Step 4: Run, expect PASS**

Run: `go test ./internal/sanitize/`
Expected: `ok`.

- [ ] **Step 5: Commit** `Sanitize: keep virtual container devices; classify device-only raw lxc lines (#141)`

### Task 2: The root@pam-only refusal (pure function)

**Files:**
- Modify: `internal/sanitize/devices.go` (or a new `rootonly.go`), with tests in the matching `_test.go`
- Delete: `RawLXCKeys` and its test once Task 3 no longer calls it (do it in Task 3)

**Interfaces:**
- Consumes: `RawLXCLines`, `DeviceOnly`, `DevicePath`, `KeptDevice`, `KeyDetail` (Task 1).
- Produces: `func RootOnly(kind model.GuestKind, orig map[string]string) (raw []string, keys []KeyDetail)`.
  - `raw` holds the sorted, distinct keys of non-device raw lines. It is nil for a VM, or when every raw line is device-only.
  - `keys` holds the blocking config keys in a stable order: `hookscript` first, then indexed keys by name and number (`dev2` before `dev10`). `Detail` is the device path for `devN`, the `host=` value or first field for `hostpciN`/`usbN`/`serialN`, and empty otherwise.

- [ ] **Step 1: Write the failing test** `TestRootOnly`, table-driven.

Refused (want key, and detail where shown):
- LXC:
  - `dev0: /dev/ttyUSB0` → `dev0`, `/dev/ttyUSB0`;
  - `dev0: /dev/net/tun2`;
  - `hookscript: local:snippets/x.sh`.
- QEMU:
  - `hookscript`;
  - `parallel0: /dev/parport0`;
  - `ivshmem: size=32`;
  - `serial0: /dev/ttyS0`;
  - `usb0: host=1234:5678`, `usb0: 1-1.2` (bare form, not spice);
  - `hostpci0: 0000:01:00.0`, `hostpci0: host=0000:01:00.0,pcie=1`, `hostpci0: mapping=gpu,romfile=x.rom`.

Not refused:
- LXC:
  - `dev0: /dev/net/tun`, `dev1: path=/dev/fuse,mode=0666`;
  - only device-only raw lines.
- QEMU:
  - `serial0: socket`;
  - `usb0: spice`, `usb0: host=spice`, `usb0: mapping=stick`;
  - `hostpci0: mapping=gpu`;
  - `rng0: source=/dev/hwrng`;
  - `virtiofs0: dirid=x`.

Raw lines:
- `lxc.apparmor.profile: unconfined` with `lxc.cgroup2.devices.allow: a` → `raw == ["lxc.apparmor.profile"]`;
- `lxc.apparmor.profile` and `lxc.idmap` → both, sorted;
- device-only only → `raw == nil`.

Order: `dev10: /dev/ttyUSB1`, `dev2: /dev/ttyUSB0` and `hookscript` give `hookscript, dev2, dev10`.

- [ ] **Step 2: Run, expect FAIL** (undefined `RootOnly`)

Run: `go test ./internal/sanitize/ -run TestRootOnly`

- [ ] **Step 3: Implement `RootOnly`.**
- Bare `hostpciN` (no `host=`/`mapping=` key) counts as `host=`.
- `usbN` uses the same reading as `isVirtualDevice`.
- Order keys like `mountKeys` in `restore.go`.

- [ ] **Step 4: Run, expect PASS**

Run: `go test ./internal/sanitize/`

- [ ] **Step 5: Commit** `Sanitize: list the keys only root@pam can restore or remove (#140)`

### Task 3: Refuse up front in the runner; the PVE fake behaves like PVE

**Files:**
- Modify: `internal/run/runner.go:623-630` (replace the raw `lxc.*` check), `internal/run/reasons.go` (new reason builder), `internal/run/runner_test.go`
- Modify: `internal/pve/pvefake/fake.go` (`Restore`, `SetGuestConfig`)
- Modify: tests that restored a guest with a refused key and expected Sanitize to remove it (`internal/run/job_test.go`, `internal/target/existing_test.go`). They now expect the refusal, or switch to a keep-listed or mapped device.
- Delete: `sanitize.RawLXCKeys` and `TestRawLXCKeys`

**Interfaces:**
- Consumes: `sanitize.RootOnly` (Task 2).
- Produces: `func rootOnlyReason(raw []string, keys []sanitize.KeyDetail) string` in `reasons.go`.
  - Keys only: `PBP cannot proof dev1 (/dev/ttyUSB0), hookscript: only root@pam can restore or remove them (` + docsToken + `)`.
  - Raw only: `PBP cannot proof raw lxc settings (lxc.apparmor.profile): only root@pam can restore or remove them (` + docsToken + `)`.
  - Both: `raw lxc settings (…)` first, then `, ` and the keys.
  - At most three keys or raw keys named per group, then `and N more`.

- [ ] **Step 1: Write the failing tests.**

In `reasons_test.go`, a `TestRootOnlyReason` covering the three shapes above and the `and N more` cap.

In `runner_test.go`, rename `TestRunRawLXCConfigIsError` → `TestRunRootOnlyIsError`, table-driven over:
- LXC `lxc.apparmor.profile`;
- LXC `dev0: /dev/ttyUSB0`;
- LXC `hookscript`;
- LXC `dev0: /dev/ttyUSB0` plus `lxc.idmap` (one combined reason);
- QEMU `hostpci0: 0000:01:00.0`;
- QEMU `serial0: /dev/ttyS0`.

Each expects:
- `ERROR` with `rootOnlyReason(…)`'s exact text;
- no restore of that guest (`f.tgt.restores()`), while the neighbour guest is restored;
- notes ending ` · ` + reason.

Add `TestRunDeviceContainerIsProofed`, an unprivileged LXC with:
- `dev0: /dev/net/tun`;
- `lxc.cgroup2.devices.allow: c 188:* rwm`;
- `lxc.mount.entry: /dev/ttyUSB0 dev/ttyUSB0 none bind,optional,create=file`.

Expect:
- it is restored;
- the copy's config (fake `Configs[vmid]`) keeps `dev0` and has no `lxc.*` key;
- `SetGuestConfig`'s delete list has no `dev0`;
- the verdict is not `ERROR`.

Add a case to `TestRunPrivilegedContainer`: a privileged container with only device-only lines and no `Sys.Modify` gets `reasonPrivileged`.

In `pvefake`, a test that:
- an LXC restore drops `lxc.*` keys and keeps `dev0`;
- `SetGuestConfig` deleting LXC `dev0` or `hookscript` returns `*pve.APIError{Status: 403}`, with message `Permission check failed (configuring device passthrough is only allowed for root@pam)`, or `(changing the hookscript is only allowed for root@pam)` for `hookscript`.

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./internal/run/ ./internal/pve/...`
Expected: undefined `rootOnlyReason`; the fake keeps `lxc.*` keys.

- [ ] **Step 3: Implement.**
- **Runner:** `if raw, keys := sanitize.RootOnly(b.Key.Kind, orig); len(raw)+len(keys) > 0 { … fail(model.Error, rootOnlyReason(raw, keys)); annotate(…) }`, at the same place and with the same notes handling as today's check, for both kinds.
- **Fake `Restore`, LXC:** delete every key with prefix `lxc.`, as PVE does for a token.
- **Fake `SetGuestConfig`, LXC:** refuse deleting or setting `devN` and `hookscript` as above, before changing anything.
- Remove `RawLXCKeys`, then fix every test the refusal or the fake changed. A test changed to expect the refusal says why in a comment.

- [ ] **Step 4: Run, expect PASS**

Run: `go test ./internal/...`
Expected: all `ok`.

- [ ] **Step 5: Commit** `Refuse guests only root@pam could restore or make safe, before restoring them (#140)`

### Task 4: The job records and logs dropped and kept devices

**Files:**
- Modify: `internal/run/job.go` (`missingPieces`, logging after restore and after Sanitize), `internal/run/job_test.go`

**Interfaces:**
- Consumes: `sanitize.MissingFromOriginal` device pieces (Task 1), `sanitize.KeptDevices` (Task 1).

- [ ] **Step 1: Write the failing tests** in `job_test.go`:
- `missingPieces` keeps two `passthrough` pieces with the same `Key` `lxc.mount.entry` but different `Path`s. Today's `Kind+Key` dedup collapses them.
- It still counts a bind mount reported by both Sanitize and the original once (existing case).
- A job for the container of `TestRunDeviceContainerIsProofed` logs:
  - after "restore done", one line: `restored without its raw lxc lines (lxc.cgroup2.devices.allow c 188:* rwm, lxc.mount.entry /dev/ttyUSB0): only root@pam can restore them`. Wording may vary; assert it contains `restored without its raw lxc lines` and `/dev/ttyUSB0`;
  - after Sanitize, `kept dev0 (/dev/net/tun) in the copy: a virtual device`.

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./internal/run/ -run 'MissingPieces|Device'`

- [ ] **Step 3: Implement.**
- Dedup: a piece whose `Key` starts with `lxc.` compares on the whole struct; others keep `Kind+Key`.
- Logging: name at most three lines in the log line, then `and N more`.

- [ ] **Step 4: Run, expect PASS**

Run: `go test ./internal/run/`

- [ ] **Step 5: Commit** `Run: log the devices a container copy was restored without, and the ones it keeps (#141)`

### Task 5: Trace failed units to dropped devices; the FAILED hint

**Files:**
- Modify: `internal/probe/missing.go` (a device step at the end of `traceMissing`), `internal/probe/linux.go` (`triage`: the hint), `internal/probe/missing_test.go`, `internal/probe/linux_test.go`

**Interfaces:**
- Consumes: `probe.Input.Missing`, which holds pieces with `Kind == "passthrough"`, `Key` prefixed `lxc.`, and a non-empty `Path` (Task 1).
- Produces:
  - `func traceDevices(ctx context.Context, ex Executor, units []string, missing []model.MissingPiece) (covered []model.NotCovered, untraced []string)`, called by `traceMissing` on the units still untraced.
  - `func deviceHint(missing []model.MissingPiece) string`: `""` when there are no `lxc.` device pieces.

- [ ] **Step 1: Write the failing tests** with the probe's scripted executor:
- Journal of `z2m.service` (`journalctl -u z2m.service -b --no-pager -n 50 -o cat`) contains `Error: open("/dev/ttyUSB0"): No such file or directory`; piece `Path: /dev/ttyUSB0`; `test -e /dev/ttyUSB0` exits 1 → covered by that piece.
- Piece `/dev/serial/by-id`, journal `cannot open /dev/serial/by-id/usb-x-port0:`, `test -e /dev/serial/by-id/usb-x-port0` exits 1 → covered.
- Same as the first, but `test -e` exits 0 → untraced.
- Piece `/dev/ttyUSB1`, journal names `/dev/ttyUSB10` → untraced.
- `journalctl` exits 1, or returns an exec error → untraced.
- With no `lxc.` device piece that has a `Path`, no `journalctl` command is executed (assert on the executor's call log).
- Allow-line pieces only (no `Path`) → no journal read, untraced.
- `triage` on a guest with device pieces and an untraced unit → the FAILED reason starts `failed: z2m.service` and contains `/dev/ttyUSB0` and `ignore_units`. With allow-line pieces only, it contains `device rules` and `ignore_units`. Without device pieces, the reason is exactly as today.

- [ ] **Step 2: Run, expect FAIL**

Run: `go test ./internal/probe/ -run 'Device|Triage'`

- [ ] **Step 3: Implement.**

**Finding paths:** `regexp.MustCompile(`/dev/[^\s"'():,;]+`)` over each journal line. Each match is explained by a piece when `pathWithin(match, piece.Path)`, and covered only when `test -e <match>` exits exactly 1.

**The hint:**
- Format: ` · the copy has no <paths> (raw lxc lines dropped); if a unit needs them, list it in checks.guests ignore_units`.
- Use `nameList` for the paths, or `device rules` when no piece has a `Path`.
- It is appended in `triage` before `r.fail`, so `bound` still caps the reason.

- [ ] **Step 4: Run, expect PASS**

Run: `go test ./internal/probe/ ./internal/run/`

- [ ] **Step 5: Commit** `Probe: trace failed units to devices the copy was restored without (#141)`

### Task 6: Docs and CHANGELOG

**Files:**
- Modify: `docs/security.md` ("Guests PBP cannot proof"), `docs/checks.md`, `docs/troubleshooting.md`, `CHANGELOG.md` (an `Unreleased` section at the top, in the existing style)

- [ ] **Step 1: Write the docs** as the spec's Docs section lists.
  - **`security.md`:**
    - Replace the raw `lxc.*` bullet: device-only lines (what counts, the character-device list) are proofed without the devices; other lines still block.
    - Replace the `devN`/hookscript/raw-passthrough bullet: the up-front refusal with its reason text; the keep-list; mapped VM devices need `Mapping.Use`.
    - Add a sentence that privileged containers with devices also need `Sys.Modify`.
  - **`checks.md`:** dropped devices in "not covered by the proof", journal tracing in one paragraph, and `checks.guests` `ignore_units` for the rest.
  - **`troubleshooting.md`:** rows for `PBP cannot proof … only root@pam can restore or remove them` and for the FAILED hint.
  - **`CHANGELOG.md`:** user-facing, Before/Now for both issues.
- [ ] **Step 2: Check** that every reason quoted in the docs matches the code's text exactly (grep each one), and that no lab details appear (`git diff main -- docs CHANGELOG.md`).
- [ ] **Step 3: Commit** `Docs: containers with devices and the up-front refusal (#140, #141)`

### After the tasks

1. The whole suite on the development host: `go test -race ./...`, `go vet ./...`, `golangci-lint run`.
2. An independent review of the branch by a Fable subagent before the PR (owner's standing rule).
3. The PR on Gitea, closing #140 and #141.
4. Live verification on the owner's test node, as the spec's last section lists, **only with the owner's go-ahead** for the temporary `Sys.Modify` grant and the throwaway container.
