package cursor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// managedProfile creates a private profile directory where the login flow puts
// them: as a direct child of the service's profiles root.
func managedProfile(t *testing.T, service *Service, name string) string {
	t.Helper()
	profiles, err := service.paths.ProfilesRoot()
	if err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(profiles, name)
	if err = os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestServiceRefusesAuthIDBeforeItsAccountIsRegistered(t *testing.T) {
	service, _ := testService(t)
	_, err := service.Execute(context.Background(), Request{AuthID: "cursor-unregistered", ConversationID: "c", Prompt: "hello"})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "unknown_auth" {
		t.Fatalf("error = %#v", err)
	}
}

func TestRegisterAccountRejectsSharedAndInsecureProfiles(t *testing.T) {
	service, _ := testService(t)
	shared := managedProfile(t, service, "shared")
	if err := os.Chmod(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterAccount(Account{AuthID: "cursor-x", ProfileDir: shared}); err == nil {
		t.Fatal("group-readable profile accepted")
	}
	if err := os.Chmod(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterAccount(Account{AuthID: "cursor-x", ProfileDir: shared}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterAccount(Account{AuthID: "cursor-y", ProfileDir: shared}); err == nil {
		t.Fatal("second account sharing one profile directory accepted")
	}
}

func TestRegisterAccountRefusesProfilesOutsideTheManagedProfilesRoot(t *testing.T) {
	service, _ := testService(t)
	profiles, err := service.paths.ProfilesRoot()
	if err != nil {
		t.Fatal(err)
	}
	root, err := service.paths.Root()
	if err != nil {
		t.Fatal(err)
	}
	// A stored auth record must not be able to aim the Cursor CLI at the host
	// auth directory, the data root, or anywhere else outside the profiles root.
	outside := map[string]string{
		"host auth directory":    root,
		"unrelated directory":    t.TempDir(),
		"profiles root itself":   profiles,
		"nested below a profile": filepath.Join(profiles, "outer", "inner"),
	}
	for name, path := range outside {
		t.Run(name, func(t *testing.T) {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o700); err != nil {
				t.Fatal(err)
			}
			_, errRegister := service.RegisterAccount(Account{AuthID: "cursor-escape", ProfileDir: path})
			if FailureCode(errRegister) != "invalid_profile" {
				t.Fatalf("profile outside the managed root accepted: %#v", errRegister)
			}
		})
	}
	if _, err = service.RegisterAccount(Account{AuthID: "cursor-ok", ProfileDir: managedProfile(t, service, "contained")}); err != nil {
		t.Fatalf("a contained profile was refused: %v", err)
	}
}

func TestRegisterAccountRemovesTheReplacedProfileOfTheSameAccount(t *testing.T) {
	service, factory := testService(t)
	first := managedProfile(t, service, "first")
	if _, err := service.RegisterAccount(Account{AuthID: "cursor-same", ProfileDir: first}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Execute(context.Background(), Request{AuthID: "cursor-same", ConversationID: "c", Prompt: "hello"}); err != nil {
		t.Fatal(err)
	}
	client := factory.clients["cursor-same"]

	second := managedProfile(t, service, "second")
	if _, err := service.RegisterAccount(Account{AuthID: "cursor-same", ProfileDir: second}); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	closed := client.closed
	client.mu.Unlock()
	if !closed {
		t.Fatal("the stale client was not closed before its profile was removed")
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("the replaced profile directory still exists: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("the new profile directory was removed: %v", err)
	}
	account, ok := service.Account("cursor-same")
	if !ok || account.ProfileDir != second {
		t.Fatalf("registered account = %#v", account)
	}
}

func TestRegisterAccountIsSafeUnderConcurrentLoginAndExecution(t *testing.T) {
	service, factory := testService(t)
	var group sync.WaitGroup
	failures := make(chan error, 16)
	for index := 0; index < 8; index++ {
		authID := "cursor-login-" + string(rune('a'+index))
		profile := managedProfile(t, service, authID)
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := service.RegisterAccount(Account{AuthID: authID, ProfileDir: profile, Label: authID}); err != nil {
				failures <- err
				return
			}
			if _, err := service.Execute(context.Background(), Request{AuthID: authID, ConversationID: authID, Prompt: "hello"}); err != nil {
				failures <- err
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	factory.mu.Lock()
	defer factory.mu.Unlock()
	for authID, client := range factory.clients {
		if filepath.Base(client.profile) != authID {
			t.Fatalf("account %q ran under profile %q", authID, client.profile)
		}
	}
}

func TestRegisterAccountRefusesAModelThatCouldBeReadAsAFlag(t *testing.T) {
	service, _ := testService(t)
	for _, model := range []string{"--dangerous", "-p", "auto model", "auto;rm", "../auto", ""} {
		account := Account{AuthID: "cursor-model", ProfileDir: managedProfile(t, service, "model"), Model: model}
		if model == "" {
			// An empty model is filled in with the default rather than refused.
			registered, err := service.RegisterAccount(account)
			if err != nil || registered.Model != DefaultModel {
				t.Fatalf("empty model = %#v %v", registered, err)
			}
			continue
		}
		if _, err := service.RegisterAccount(account); err == nil {
			t.Fatalf("model %q accepted", model)
		}
	}
}
