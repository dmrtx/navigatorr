package smbprobe

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

func readPasswordFile(path string) (string, error) {
	f, err := openSecretFile(path)
	if err != nil {
		return "", fmt.Errorf("opening password_file safely: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("statting password_file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("password_file must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("password_file permissions %04o are too broad; require 0600 or stricter", info.Mode().Perm())
	}
	data, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	if err != nil {
		return "", fmt.Errorf("reading password_file: %w", err)
	}
	if len(data) > 64*1024 {
		return "", errors.New("password_file exceeds 64 KiB")
	}
	password := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if password == "" || strings.ContainsRune(password, '\x00') {
		return "", errors.New("password_file contains an empty or invalid password")
	}
	return password, nil
}

func randomToken() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generating probe token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
