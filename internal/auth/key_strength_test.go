package auth_test

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Busnes-app/kyyard-server/internal/auth"
	appcrypto "github.com/Busnes-app/kyyard-server/internal/crypto"
)

func TestTOTPKeyFloorPreservesSealedLegacySecrets(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	for _, size := range []int{1, 15, 16, 20} {
		raw := []byte(strings.Repeat("s", size))
		secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
		sealed, err := appcrypto.EncryptAESGCM([]byte(secret), key)
		if err != nil {
			t.Fatal(err)
		}
		opened, err := appcrypto.DecryptAESGCM(sealed, key)
		if err != nil || string(opened) != secret {
			t.Fatalf("legacy secret changed at length %d", size)
		}
		// Independently compute RFC 4226/6238, including keys the production
		// generator must refuse, rather than asking it to generate the fixture.
		var counter [8]byte
		binary.BigEndian.PutUint64(counter[:], uint64(time.Now().Unix()/30))
		mac := hmac.New(sha1.New, raw)
		mac.Write(counter[:])
		digest := mac.Sum(nil)
		offset := digest[len(digest)-1] & 15
		value := binary.BigEndian.Uint32(digest[offset:offset+4]) & 0x7fffffff
		code := fmt.Sprintf("%06d", value%1000000)
		if _, ok := auth.ValidateTOTP(string(opened), code); ok != (size >= 16) {
			t.Errorf("length %d: accepted=%v", size, ok)
		}
	}
}
