package store

import (
	"context"
	"reflect"
	"testing"
)

// Tags are matched exactly when targeting, so near-identical input must not
// become several different groups.
func TestNormaliseTags(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"lowercased and trimmed", []string{"Web", " DB ", "db"}, []string{"db", "web"}},
		{"duplicates collapse", []string{"web", "web", "WEB"}, []string{"web"}},
		{"empties dropped", []string{"", "  ", "web"}, []string{"web"}},
		{"commas stripped", []string{"a,b"}, []string{"ab"}},
		{"sorted for stable display", []string{"z", "a", "m"}, []string{"a", "m", "z"}},
		{"nothing in, nothing out", nil, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormaliseTags(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("NormaliseTags(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSetAndListTags(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	a := mustDevice(t, st, "web-01", "linux")
	b := mustDevice(t, st, "web-02", "linux")
	c := mustDevice(t, st, "win-01", "windows")

	if _, err := st.SetDeviceTags(ctx, a, []string{"Web", "site-a"}); err != nil {
		t.Fatalf("set tags: %v", err)
	}
	if _, err := st.SetDeviceTags(ctx, b, []string{"web"}); err != nil {
		t.Fatalf("set tags: %v", err)
	}
	if _, err := st.SetDeviceTags(ctx, c, []string{"site-a"}); err != nil {
		t.Fatalf("set tags: %v", err)
	}

	tags, err := st.ListTags(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	// Most common first.
	want := []TagCount{{"site-a", 2}, {"web", 2}}
	if !reflect.DeepEqual(tags, want) {
		t.Errorf("ListTags() = %v, want %v", tags, want)
	}

	dev, err := st.GetDevice(ctx, a)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if !reflect.DeepEqual(dev.Tags, []string{"site-a", "web"}) {
		t.Errorf("device tags = %v, want [site-a web]", dev.Tags)
	}

	// Setting replaces rather than merges.
	if _, err := st.SetDeviceTags(ctx, a, []string{"other"}); err != nil {
		t.Fatalf("replace tags: %v", err)
	}
	dev, _ = st.GetDevice(ctx, a)
	if !reflect.DeepEqual(dev.Tags, []string{"other"}) {
		t.Errorf("tags after replace = %v, want [other]", dev.Tags)
	}

	if _, err := st.SetDeviceTags(ctx, "nosuchdevice", []string{"x"}); err != ErrNotFound {
		t.Errorf("tagging unknown device = %v, want ErrNotFound", err)
	}
}

// Naming devices explicitly is precise, so an incompatible one is an error.
// Targeting a group is broad, so incompatible ones are skipped — but reported.
func TestDispatchManySkipsRatherThanFails(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	linux := mustDevice(t, st, "ubuntu-01", "linux")
	mac := mustDevice(t, st, "mac-01", "darwin")
	win := mustDevice(t, st, "win-01", "windows")
	script := mustScript(t, st, "df", InterpreterSh, "df -h")

	jobs, skipped, err := st.DispatchMany(ctx, script.ID, []string{linux, mac, win}, "tester")
	if err != nil {
		t.Fatalf("dispatch many: %v", err)
	}
	if len(jobs) != 2 {
		t.Errorf("queued %d jobs, want 2 (linux and darwin)", len(jobs))
	}
	if len(skipped) != 1 || skipped[0].Hostname != "win-01" {
		t.Fatalf("skipped = %+v, want just win-01", skipped)
	}
	if skipped[0].Reason == "" {
		t.Error("a skipped device must say why")
	}

	// The single-device path stays strict.
	if _, err := st.Dispatch(ctx, script.ID, []string{win}, "tester"); err == nil {
		t.Error("Dispatch to an incompatible device: want error, got nil")
	}
}

func TestDispatchManyRejectsWhenNothingIsEligible(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	win := mustDevice(t, st, "win-01", "windows")
	script := mustScript(t, st, "df", InterpreterSh, "df -h")

	// Failing loudly beats queueing nothing and reporting success.
	_, skipped, err := st.DispatchMany(ctx, script.ID, []string{win}, "tester")
	if err == nil {
		t.Error("want an error when no selected device can run the script")
	}
	if len(skipped) != 1 {
		t.Errorf("skipped = %v, want the incompatible device reported", skipped)
	}

	if _, _, err := st.DispatchMany(ctx, script.ID, nil, "tester"); err == nil {
		t.Error("want an error with no devices selected")
	}
}

func TestDispatchManySkipsRetiringDevices(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	ok := mustDevice(t, st, "ubuntu-01", "linux")
	going := mustDevice(t, st, "ubuntu-02", "linux")
	script := mustScript(t, st, "df", InterpreterSh, "df -h")

	if err := st.RetireDevice(ctx, going); err != nil {
		t.Fatalf("retire: %v", err)
	}

	jobs, skipped, err := st.DispatchMany(ctx, script.ID, []string{ok, going}, "tester")
	if err != nil {
		t.Fatalf("dispatch many: %v", err)
	}
	if len(jobs) != 1 || jobs[0].DeviceID != ok {
		t.Errorf("queued %d jobs, want 1 for the live device", len(jobs))
	}
	if len(skipped) != 1 || skipped[0].Reason != "being retired" {
		t.Errorf("skipped = %+v, want the retiring device", skipped)
	}
}
