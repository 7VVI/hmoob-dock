package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// The cost parameters of a new password hash.
const (
	argonMemoryKiB = 64 * 1024
	argonTime      = 2
	argonThreads   = 1
	argonSaltSize  = 16
	argonKeySize   = 32
)

// The bounds of the parameters that are read back from a stored hash. A hash
// with parameters outside of these bounds is rejected instead of being
// computed, so a damaged record cannot make the control端 allocate an
// arbitrary amount of memory.
const (
	minArgonMemoryKiB = 8
	maxArgonMemoryKiB = 1024 * 1024
	minArgonTime      = 1
	maxArgonTime      = 10
	minArgonThreads   = 1
	maxArgonThreads   = 16
)

// The length limits of a password in characters.
const (
	MinPasswordLength = 6
	MaxPasswordLength = 128
)

// argon2idVersion is the version of the Argon2 encoding.
const argon2idVersion = 19

// ValidatePassword reports whether the password meets the policy of the
// control端.
func ValidatePassword(password string) error {
	length := utf8.RuneCountInString(password)
	if length < MinPasswordLength || length > MaxPasswordLength {
		return fmt.Errorf("%w: %d characters, allowed are %d to %d",
			ErrPasswordPolicy, length, MinPasswordLength, MaxPasswordLength)
	}
	return nil
}

// EncodePassword hashes the password with Argon2id and returns the encoded
// form, which carries the salt and the parameters of the hash.
func EncodePassword(password string) (string, error) {
	salt := make([]byte, argonSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate the salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeySize)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2idVersion,
		argonMemoryKiB,
		argonTime,
		argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether the password matches the encoded hash. The
// comparison of the derived key takes the same time for every value.
func VerifyPassword(encoded, password string) error {
	params, salt, expected, err := decodePasswordHash(encoded)
	if err != nil {
		return err
	}
	key := argon2.IDKey([]byte(password), salt, params.time, params.memoryKiB, params.threads, uint32(len(expected)))
	if subtle.ConstantTimeCompare(key, expected) != 1 {
		return ErrInvalidPassword
	}
	return nil
}

// argon2Parameters are the cost parameters of a stored hash.
type argon2Parameters struct {
	memoryKiB uint32
	time      uint32
	threads   uint8
}

// decodePasswordHash reads the parameters, the salt and the key of an
// encoded hash. The parameters are checked against the bounds of this
// package, so a damaged record cannot ask for an arbitrary amount of memory
// and time.
func decodePasswordHash(encoded string) (argon2Parameters, []byte, []byte, error) {
	var params argon2Parameters

	// The encoded form is $argon2id$v=19$m=...,t=...,p=...$salt$key.
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return params, nil, nil, errors.New("auth: the password hash is malformed")
	}
	if parts[2] != fmt.Sprintf("v=%d", argon2idVersion) {
		return params, nil, nil, errors.New("auth: the password hash uses an unsupported version")
	}
	memory, timeCost, threads, err := parseArgon2Parameters(parts[3])
	if err != nil {
		return params, nil, nil, err
	}
	params = argon2Parameters{memoryKiB: memory, time: timeCost, threads: threads}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return params, nil, nil, fmt.Errorf("auth: the password hash has a malformed salt: %w", err)
	}
	if len(salt) < 8 || len(salt) > 64 {
		return params, nil, nil, errors.New("auth: the password hash has an unexpected salt length")
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return params, nil, nil, fmt.Errorf("auth: the password hash has a malformed key: %w", err)
	}
	if len(key) < 16 || len(key) > 64 {
		return params, nil, nil, errors.New("auth: the password hash has an unexpected key length")
	}
	return params, salt, key, nil
}

// parseArgon2Parameters reads the cost parameters of an encoded hash and
// checks them against the bounds of this package.
func parseArgon2Parameters(raw string) (uint32, uint32, uint8, error) {
	var memory, timeCost uint32
	var threads uint8

	for _, field := range strings.Split(raw, ",") {
		name, value, found := strings.Cut(field, "=")
		if !found {
			return 0, 0, 0, errors.New("auth: the password hash has a malformed parameter")
		}
		number, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("auth: the password hash has a malformed parameter: %w", err)
		}
		switch name {
		case "m":
			if number < minArgonMemoryKiB || number > maxArgonMemoryKiB {
				return 0, 0, 0, errors.New("auth: the memory parameter of the password hash is out of range")
			}
			memory = uint32(number)
		case "t":
			if number < minArgonTime || number > maxArgonTime {
				return 0, 0, 0, errors.New("auth: the time parameter of the password hash is out of range")
			}
			timeCost = uint32(number)
		case "p":
			if number < minArgonThreads || number > maxArgonThreads {
				return 0, 0, 0, errors.New("auth: the thread parameter of the password hash is out of range")
			}
			threads = uint8(number)
		default:
			return 0, 0, 0, fmt.Errorf("auth: the password hash has an unknown parameter %q", name)
		}
	}
	if memory == 0 || timeCost == 0 || threads == 0 {
		return 0, 0, 0, errors.New("auth: the password hash is missing a parameter")
	}
	if memory < uint32(8*threads) {
		return 0, 0, 0, errors.New("auth: the memory parameter of the password hash is too small for its threads")
	}
	return memory, timeCost, threads, nil
}
