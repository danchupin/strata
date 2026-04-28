package workers

import (
	"context"
	"errors"
	"testing"
)

func TestRegisterAndLookup(t *testing.T) {
	t.Cleanup(Reset)
	Reset()

	built := false
	Register(Worker{
		Name: "alpha",
		Build: func(deps Dependencies) (Runner, error) {
			built = true
			return RunnerFunc(func(ctx context.Context) error { return nil }), nil
		},
	})

	got, ok := Lookup("alpha")
	if !ok {
		t.Fatal("Lookup(alpha) not found after Register")
	}
	if got.Name != "alpha" {
		t.Fatalf("Lookup returned wrong name: %q", got.Name)
	}
	if _, err := got.Build(Dependencies{}); err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if !built {
		t.Fatal("Build hook never ran")
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	t.Cleanup(Reset)
	Reset()

	Register(Worker{Name: "dup", Build: func(Dependencies) (Runner, error) { return nil, nil }})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate Register")
		}
	}()
	Register(Worker{Name: "dup", Build: func(Dependencies) (Runner, error) { return nil, nil }})
}

func TestRegisterNilBuildPanics(t *testing.T) {
	t.Cleanup(Reset)
	Reset()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil Build")
		}
	}()
	Register(Worker{Name: "nil-build"})
}

func TestRegisterEmptyNamePanics(t *testing.T) {
	t.Cleanup(Reset)
	Reset()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty Name")
		}
	}()
	Register(Worker{Build: func(Dependencies) (Runner, error) { return nil, nil }})
}

func TestNamesSorted(t *testing.T) {
	t.Cleanup(Reset)
	Reset()

	for _, n := range []string{"gamma", "alpha", "beta"} {
		Register(Worker{Name: n, Build: func(Dependencies) (Runner, error) { return nil, nil }})
	}
	got := Names()
	want := []string{"alpha", "beta", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("Names len: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestResolveUnknownNameFailsImmediately(t *testing.T) {
	t.Cleanup(Reset)
	Reset()
	Register(Worker{Name: "gc", Build: func(Dependencies) (Runner, error) { return nil, nil }})

	_, err := Resolve([]string{"gc", "ghost"})
	if err == nil {
		t.Fatal("expected error for unknown worker")
	}
}

func TestResolveOrderPreserved(t *testing.T) {
	t.Cleanup(Reset)
	Reset()
	for _, n := range []string{"a", "b", "c"} {
		Register(Worker{Name: n, Build: func(Dependencies) (Runner, error) { return nil, nil }})
	}
	got, err := Resolve([]string{"c", "a", "b"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"c", "a", "b"}
	for i := range want {
		if got[i].Name != want[i] {
			t.Fatalf("Resolve[%d]: got %q want %q", i, got[i].Name, want[i])
		}
	}
}

func TestResolveEmptyReturnsEmpty(t *testing.T) {
	t.Cleanup(Reset)
	Reset()
	got, err := Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %d", len(got))
	}
}

func TestRunnerFuncSatisfiesRunner(t *testing.T) {
	called := false
	var r Runner = RunnerFunc(func(ctx context.Context) error {
		called = true
		return errors.New("boom")
	})
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("expected error from RunnerFunc")
	}
	if !called {
		t.Fatal("RunnerFunc not invoked")
	}
}
