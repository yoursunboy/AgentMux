package storage

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestSettingSetAndGet(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.Settings().Set(ctx, SettingInstallID, "inst_abc"); err != nil {
		t.Fatalf("Set returned an error: %v", err)
	}
	got, err := store.Settings().Get(ctx, SettingInstallID)
	if err != nil {
		t.Fatalf("Get returned an error: %v", err)
	}
	if got != "inst_abc" {
		t.Errorf("Get = %q, want %q", got, "inst_abc")
	}
}

func TestSettingSetReplacesThePreviousValue(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	for _, value := range []string{"first", "second", "third"} {
		if err := store.Settings().Set(ctx, "theme", value); err != nil {
			t.Fatalf("Set(%q) returned an error: %v", value, err)
		}
	}

	got, err := store.Settings().Get(ctx, "theme")
	if err != nil {
		t.Fatalf("Get returned an error: %v", err)
	}
	if got != "third" {
		t.Errorf("Get = %q, want the most recent value %q", got, "third")
	}

	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'theme'`).Scan(&count); err != nil {
		t.Fatalf("could not count rows: %v", err)
	}
	if count != 1 {
		t.Errorf("the settings table holds %d rows for one key, want 1", count)
	}
}

func TestSettingGetForAMissingKey(t *testing.T) {
	store := newTestStore(t)

	got, err := store.Settings().Get(context.Background(), "never.set")
	if !errors.Is(err, ErrSettingNotFound) {
		t.Fatalf("Get failed with %v, want ErrSettingNotFound", err)
	}
	if got != "" {
		t.Errorf("Get = %q alongside an error, want an empty string", got)
	}
}

// TestGetOrCreateGeneratesOnce is the property the install identity depends
// on: the value must be stable for the life of the installation.
func TestGetOrCreateGeneratesOnce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	var calls int
	generate := func() (string, error) {
		calls++
		return "inst_generated", nil
	}

	first, err := store.Settings().GetOrCreate(ctx, SettingInstallID, generate)
	if err != nil {
		t.Fatalf("the first GetOrCreate returned an error: %v", err)
	}
	if first != "inst_generated" {
		t.Errorf("GetOrCreate = %q, want the generated value", first)
	}

	second, err := store.Settings().GetOrCreate(ctx, SettingInstallID, generate)
	if err != nil {
		t.Fatalf("the second GetOrCreate returned an error: %v", err)
	}
	if second != first {
		t.Errorf("GetOrCreate = %q on the second call, want %q", second, first)
	}
	if calls != 1 {
		t.Errorf("the generator ran %d times, want 1: the value must be generated once and kept", calls)
	}
}

func TestGetOrCreateReturnsAnExistingValue(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.Settings().Set(ctx, SettingInstallID, "inst_existing"); err != nil {
		t.Fatalf("Set returned an error: %v", err)
	}

	got, err := store.Settings().GetOrCreate(ctx, SettingInstallID, func() (string, error) {
		t.Error("the generator ran although the setting already existed")
		return "inst_new", nil
	})
	if err != nil {
		t.Fatalf("GetOrCreate returned an error: %v", err)
	}
	if got != "inst_existing" {
		t.Errorf("GetOrCreate = %q, want the stored value %q", got, "inst_existing")
	}
}

func TestGetOrCreateRejectsABadGenerator(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// No generator at all.
	if _, err := store.Settings().GetOrCreate(ctx, "missing", nil); err == nil {
		t.Error("GetOrCreate without a generator succeeded, want a rejection")
	}

	// A generator that produces nothing usable.
	for _, value := range []string{"", "   "} {
		if _, err := store.Settings().GetOrCreate(ctx, "missing", func() (string, error) {
			return value, nil
		}); err == nil {
			t.Errorf("GetOrCreate accepted the generated value %q, want a rejection", value)
		}
	}

	// A failed generation must not be stored.
	if _, err := store.Settings().GetOrCreate(ctx, "missing", func() (string, error) {
		return "", errors.New("entropy source unavailable")
	}); err == nil {
		t.Error("GetOrCreate ignored a generator failure")
	}
	if _, err := store.Settings().Get(ctx, "missing"); !errors.Is(err, ErrSettingNotFound) {
		t.Errorf("a failed generation stored something: Get returned %v", err)
	}
}

// TestGetOrCreateStoreFailureIsReported checks the read error is not mistaken
// for a missing key, which would overwrite a value that exists.
func TestGetOrCreateStoreFailureIsReported(t *testing.T) {
	store := newTestStore(t)
	store.db.Close()

	if _, err := store.Settings().GetOrCreate(context.Background(), SettingInstallID, func() (string, error) {
		t.Error("the generator ran although the store was unreachable")
		return "x", nil
	}); err == nil {
		t.Error("GetOrCreate succeeded against a closed database, want a failure")
	}
}

// TestSettingsAreIndependentPerDatabase keeps two installations from sharing
// an identity.
func TestSettingsAreIndependentPerDatabase(t *testing.T) {
	first := newTestStore(t)
	second := newTestStore(t)
	ctx := context.Background()

	if err := first.Settings().Set(ctx, SettingInstallID, "inst_one"); err != nil {
		t.Fatalf("Set returned an error: %v", err)
	}
	if _, err := second.Settings().Get(ctx, SettingInstallID); !errors.Is(err, ErrSettingNotFound) {
		t.Errorf("a second database saw the first one's setting: %v", err)
	}
}

// TestSettingsSurviveConcurrentWriters checks the store tolerates the modest
// concurrency a self-hosted server sees, rather than failing with a lock.
func TestSettingsSurviveConcurrentWriters(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.Settings().Set(ctx, "counter", "value"); err != nil {
				t.Errorf("a concurrent Set failed: %v", err)
			}
		}()
	}
	wg.Wait()

	if _, err := store.Settings().Get(ctx, "counter"); err != nil {
		t.Errorf("Get after concurrent writes failed: %v", err)
	}
}
