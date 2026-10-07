// Package workload obtains and projects short-lived Keycloak service tokens.
// It never signs tokens or assigns receiver permissions.
package workload

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

type Config struct {
	Issuer           string `json:"issuer"`
	Audience         string `json:"audience"`
	ClientID         string `json:"client_id"`
	Subject          string `json:"subject"`
	ClientSecretFile string `json:"client_secret_file"`
	TokenFile        string `json:"token_file"`
}

var realmPath = regexp.MustCompile(`^(/[-a-zA-Z0-9_]+)*/realms/[-a-zA-Z0-9_]+$`)

func ReadConfig(path string) (Config, error) {
	var config Config
	body, err := readPrivateFile(path, 16*1024)
	if err != nil {
		return config, errors.New("workload configuration is unreadable")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil || decoder.Decode(new(any)) != io.EOF {
		return Config{}, errors.New("workload configuration is invalid")
	}
	return config, config.validate()
}

func (c Config) validate() error {
	u, err := url.Parse(c.Issuer)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || !realmPath.MatchString(u.Path) {
		return errors.New("workload issuer must be a canonical HTTPS Keycloak realm URL")
	}
	for _, value := range []string{c.Audience, c.ClientID, c.Subject} {
		if value == "" || len(value) > 256 || strings.ContainsAny(value, " \t\r\n") {
			return errors.New("workload audience, client and subject are required")
		}
	}
	for _, path := range []string{c.ClientSecretFile, c.TokenFile} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("workload file paths must be canonical and absolute")
		}
		if err := privateDirectory(filepath.Dir(path)); err != nil {
			return err
		}
	}
	// Readers mount the whole output directory to observe atomic replacement.
	// It must not expose the bootstrap secret, including via a parent mount.
	relative, err := filepath.Rel(filepath.Dir(c.TokenFile), c.ClientSecretFile)
	if err != nil || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return errors.New("workload bootstrap secret must be outside the projected directory")
	}
	return nil
}

func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0) {
		return errors.New("workload directory must exist and disallow group or other writes")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return errors.New("workload directory must not contain symlinks")
	}
	return nil
}

func readPrivateFile(path string, maximum int64) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("invalid workload file path")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o037 != 0) || info.Size() > maximum {
		return nil, errors.New("unsafe workload file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("unreadable workload file")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || (runtime.GOOS != "windows" && opened.Mode().Perm()&0o037 != 0) {
		return nil, errors.New("workload file changed while opening")
	}
	body, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || len(body) == 0 || int64(len(body)) > maximum {
		return nil, errors.New("invalid workload file content")
	}
	return body, nil
}

func publish(path string, body []byte) error {
	directory := filepath.Dir(path)
	if err := privateDirectory(directory); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o037 != 0) {
			return errors.New("unsafe workload destination")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("unreadable workload destination")
	}
	file, err := os.CreateTemp(directory, ".token-*")
	if err != nil {
		return errors.New("workload temporary file creation failed")
	}
	name := file.Name()
	defer os.Remove(name)
	defer file.Close()
	if _, err := file.Write(body); err != nil {
		return errors.New("workload token write failed")
	}
	if err := file.Sync(); err != nil {
		return errors.New("workload token sync failed")
	}
	if err := file.Close(); err != nil {
		return errors.New("workload token close failed")
	}
	if err := os.Rename(name, path); err != nil {
		return errors.New("workload token replacement failed")
	}
	return nil
}
