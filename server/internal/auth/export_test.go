package auth

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

func TestExportCredentialsConcurrentRefresh(t *testing.T) {
	a := &Auth{UID: "synthetic", AccessToken: "pair-0", RefreshToken: "pair-0"}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 200; i++ {
			a.Lock()
			a.AccessToken = fmt.Sprintf("pair-%d", i)
			a.RefreshToken = a.AccessToken
			a.Unlock()
		}
	}()
	for i := 0; i < 200; i++ {
		raw, err := a.ExportCredentials()
		if err != nil {
			t.Fatal(err)
		}
		var d struct {
			Auth struct {
				Access  string `json:"accessToken"`
				Refresh string `json:"refreshToken"`
			} `json:"auth"`
		}
		if json.Unmarshal(raw, &d) != nil || d.Auth.Access != d.Auth.Refresh {
			t.Fatal("mixed credential snapshot")
		}
	}
	wg.Wait()
}
