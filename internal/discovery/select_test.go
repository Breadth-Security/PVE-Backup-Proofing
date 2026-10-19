package discovery

import (
	"reflect"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/notes"
)

var ref = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func bk(storage string, kind model.GuestKind, vmid int, t time.Time) model.Backup {
	return model.Backup{Storage: storage, VolID: storage + ":" + string(kind) + "/" + t.Format(time.RFC3339), Key: model.GuestKey{Kind: kind, VMID: vmid}, Time: t}
}

func TestNewestPerGuestAcrossStorages(t *testing.T) {
	older := bk("PBS-Baremetal", model.KindLXC, 101, ref.Add(-48*time.Hour))
	newer := bk("PBS-qemu", model.KindLXC, 101, ref.Add(-24*time.Hour))
	got := NewestPerGuest([]model.Backup{older, newer})
	if len(got) != 1 || got[0] != newer {
		t.Fatalf("got %+v, want only %+v", got, newer)
	}
}

func TestNewestPerGuestKindsDistinct(t *testing.T) {
	q := bk("s", model.KindQEMU, 100, ref)
	l := bk("s", model.KindLXC, 100, ref)
	got := NewestPerGuest([]model.Backup{q, l})
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
}

func TestNewestPerGuestTieFirstStorageWins(t *testing.T) {
	a := bk("A", model.KindQEMU, 100, ref)
	b := bk("B", model.KindQEMU, 100, ref)
	got := NewestPerGuest([]model.Backup{a, b})
	if len(got) != 1 || got[0].Storage != "A" {
		t.Fatalf("got %+v, want storage A", got)
	}
}

func TestNewestPerGuestDoesNotMutateInput(t *testing.T) {
	in := []model.Backup{bk("A", model.KindQEMU, 100, ref.Add(-time.Hour)), bk("B", model.KindQEMU, 100, ref)}
	cp := append([]model.Backup(nil), in...)
	NewestPerGuest(in)
	if !reflect.DeepEqual(in, cp) {
		t.Fatal("input mutated")
	}
}

func TestOrder(t *testing.T) {
	q100 := model.GuestKey{Kind: model.KindQEMU, VMID: 100}
	l101 := model.GuestKey{Kind: model.KindLXC, VMID: 101}
	l109 := model.GuestKey{Kind: model.KindLXC, VMID: 109}
	in := []model.Backup{bk("s", model.KindQEMU, 100, ref), bk("s", model.KindLXC, 101, ref), bk("s", model.KindLXC, 109, ref)}
	cp := append([]model.Backup(nil), in...)
	last := map[model.GuestKey]time.Time{q100: ref.Add(-24 * time.Hour), l101: ref.Add(-72 * time.Hour)}
	got := Order(in, last)
	want := []model.GuestKey{l109, l101, q100}
	for i, w := range want {
		if got[i].Key != w {
			t.Fatalf("pos %d: got %v want %v", i, got[i].Key, w)
		}
	}
	if !reflect.DeepEqual(in, cp) {
		t.Fatal("input mutated")
	}
}

func TestOrderTiesDeterministic(t *testing.T) {
	in := []model.Backup{
		bk("s", model.KindQEMU, 7, ref), bk("s", model.KindLXC, 7, ref),
		bk("s", model.KindQEMU, 5, ref), bk("s", model.KindQEMU, 3, ref),
	}
	last := map[model.GuestKey]time.Time{
		{Kind: model.KindQEMU, VMID: 7}: ref, {Kind: model.KindLXC, VMID: 7}: ref, {Kind: model.KindQEMU, VMID: 5}: ref,
	}
	got := Order(in, last)
	want := []model.GuestKey{{Kind: model.KindQEMU, VMID: 3}, {Kind: model.KindQEMU, VMID: 5}, {Kind: model.KindLXC, VMID: 7}, {Kind: model.KindQEMU, VMID: 7}}
	for i, w := range want {
		if got[i].Key != w {
			t.Fatalf("pos %d: got %v want %v", i, got[i].Key, w)
		}
	}
}

func TestAlreadyProven(t *testing.T) {
	proven := model.Backup{Notes: notes.Merge("web", notes.Line(model.Proven, ref, "booted ok"))}
	if !AlreadyProven(proven, false) {
		t.Error("proven, no reproof: want true")
	}
	if AlreadyProven(proven, true) {
		t.Error("proven, reproof: want false")
	}
	if AlreadyProven(model.Backup{Notes: "plain"}, false) {
		t.Error("unproven: want false")
	}
}

// Backups of PBP's own temporary guests (VMIDs in the reserved range, e.g.
// from a vzdump job that does not exclude them) are never proofed.
func TestOutsideRange(t *testing.T) {
	a := bk("s", model.KindQEMU, 101, ref)
	in1 := bk("s", model.KindQEMU, 990000, ref)
	in2 := bk("s", model.KindQEMU, 990000, ref.Add(-time.Hour))
	in3 := bk("t", model.KindLXC, 990049, ref)
	b := bk("s", model.KindLXC, 990050, ref)
	kept, skipped := OutsideRange([]model.Backup{a, in1, in2, in3, b}, 990000, 990049)
	if !reflect.DeepEqual(kept, []model.Backup{a, b}) {
		t.Errorf("kept = %+v", kept)
	}
	want := []model.GuestKey{{Kind: model.KindQEMU, VMID: 990000}, {Kind: model.KindLXC, VMID: 990049}}
	if !reflect.DeepEqual(skipped, want) {
		t.Errorf("skipped = %+v, want each guest once", skipped)
	}
}
