package cmd

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/koding/kite/kitekey"
)

type Install struct{}

func NewInstall() *Install {
	return &Install{}
}

func (*Install) Definition() string {
	return "Install a kite from repository. Example: github.com/cenkalti/math.kite"
}

const S3URL = "http://koding-kites.s3.amazonaws.com/"

func (*Install) Exec(args []string) error {
	if len(args) != 1 {
		return errors.New("You should give a URL. Example: github.com/cenkalti/math.kite")
	}

	repoName := args[0]

	// Download manifest
	fmt.Println("Downloading manifest file...")
	manifest, err := getManifest(repoName)
	if err != nil {
		return err
	}

	version, err := getVersion(manifest)
	if err != nil {
		return err
	}

	fmt.Printf("Found version: %s\n", version)

	binaryURL, err := getBinaryURL(manifest)
	if err != nil {
		return err
	}

	// Make download request to the kite binary
	fmt.Println("Downloading kite...")
	targz, err := http.Get(binaryURL)
	if err != nil {
		return err
	}
	defer targz.Body.Close()

	// Extract gzip
	gz, err := gzip.NewReader(targz.Body)
	if err != nil {
		return err
	}
	defer gz.Close()

	// Extract tar
	tempKitePath, err := ioutil.TempDir("", "kite-install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempKitePath)

	err = extractTar(gz, tempKitePath)
	if err != nil {
		return err
	}

	bundlePath, err := validatePackage(tempKitePath, repoName)
	if err != nil {
		return err
	}

	err = installKite(bundlePath, repoName, version)
	if err != nil {
		return err
	}

	fmt.Println("Installed successfully:", filepath.Join(repoName, version))
	return nil
}

func getManifest(repoName string) (map[string]interface{}, error) {
	if !strings.HasPrefix(repoName, "github.com/") {
		return nil, errors.New("Repo other than github.com is not supported for now")
	}

	repoName = strings.TrimRight(repoName, "/")
	manifestURL := "http://raw." + repoName + "/master/.kite.json"

	res, err := http.Get(manifestURL)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode == 404 {
		return nil, errors.New("Package is not found on the server.")
	}

	if res.StatusCode != 200 {
		return nil, fmt.Errorf("Unexpected response from server: %d", res.StatusCode)
	}

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("cannot read response: %s", err.Error())
	}

	manifest := make(map[string]interface{})
	err = json.Unmarshal(body, &manifest)
	if err != nil {
		return nil, fmt.Errorf("invalid manifest file: %s", err.Error())
	}

	return manifest, nil
}

func getBinaryURL(manifest map[string]interface{}) (string, error) {
	platforms, ok := manifest["platforms"].(map[string]interface{})
	if !ok {
		return "", errors.New("no platforms key in kite manifest")
	}

	platform := runtime.GOOS + "_" + runtime.GOARCH

	platformURL, ok := platforms[platform]
	if !ok {
		return "", fmt.Errorf("no binary available for platform: %s", platform)
	}

	binaryURL, ok := platformURL.(string)
	if !ok {
		return "", errors.New("invalid platform URL")
	}

	return binaryURL, nil
}

func getVersion(manifest map[string]interface{}) (string, error) {
	version, ok := manifest["version"].(string)
	if !ok {
		return "", errors.New("invalid version string")
	}

	return version, nil
}

// extractTar reads from the io.Reader and writes the files into the directory.
func extractTar(r io.Reader, dir string) error {
	first := true // true if we are on the first entry of tarball
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			// end of tar archive
			break
		}
		if err != nil {
			return err
		}

		// Check if the same kite version is installed before
		if first {
			first = false
			kiteName := strings.TrimSuffix(hdr.Name, ".kite/")

			installed, err := isInstalled(kiteName)
			if err != nil {
				return err
			}

			if installed {
				return fmt.Errorf("Already installed: %s", kiteName)
			}
		}

		path := filepath.Join(dir, hdr.Name)

		if hdr.FileInfo().IsDir() {
			os.MkdirAll(path, 0700)
		} else {
			mode := 0600
			if isBinaryFile(hdr.Name) {
				mode = 0700
			}

			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(mode))
			if err != nil {
				return err
			}

			if _, err := io.Copy(f, tr); err != nil {
				return err
			}
		}
	}
	return nil
}

// validatePackage does some checks on kite bundle and returns the bundle path.
func validatePackage(tempKitePath, repoName string) (bundlePath string, err error) {
	dirs, err := ioutil.ReadDir(tempKitePath)
	if err != nil {
		return "", err
	}

	if len(dirs) != 1 {
		return "", errors.New("Invalid package: Package must contain only one directory.")
	}

	bundlePath = filepath.Join(tempKitePath, dirs[0].Name())

	parts := strings.Split(repoName, "/")
	if len(parts) == 0 {
		return "", errors.New("invalid repo URL")
	}

	kiteName := strings.TrimSuffix(parts[len(parts)-1], ".kite")

	_, err = os.Stat(filepath.Join(bundlePath, "bin", kiteName))
	return bundlePath, err
}

// installKite moves the .kite bundle into ~/kd/kites.
func installKite(bundlePath, repoName, version string) error {
	kiteHome, err := kitekey.KiteHome()
	if err != nil {
		return err
	}

	kitesPath := filepath.Join(kiteHome, "kites")
	repoPath := filepath.Join(kitesPath, repoName)
	versionPath := filepath.Join(repoPath, version)

	os.MkdirAll(repoPath, 0700)
	return os.Rename(bundlePath, versionPath)
}

// splitVersion takes a name like "asdf-1.2.3" and
// returns the name "asdf" and version "1.2.3" seperately.
// If allowLatest is true, then the version must not be numeric and can be "latest".
func splitVersion(fullname string, allowLatest bool) (name, version string, err error) {
	notFound := errors.New("name does not contain a version number")

	parts := strings.Split(fullname, "-")
	n := len(parts)
	if n < 2 {
		return "", "", notFound
	}

	name = strings.Join(parts[:n-1], "-")
	version = parts[n-1]

	if allowLatest && version == "latest" {
		return name, version, nil
	}

	versionParts := strings.Split(version, ".")
	for _, v := range versionParts {
		if _, err := strconv.Atoi(v); err != nil {
			return "", "", notFound
		}
	}

	return name, version, nil
}

// isBinaryFile returns true if the path is the path of the binary file
// in aplication bundle. Example: fs-0.0.1.kite/bin/fs
func isBinaryFile(path string) bool {
	parts := strings.Split(path, string(os.PathSeparator))
	if len(parts) != 3 {
		return false
	}

	binPath, err := getBinPath(parts[0])
	if err != nil {
		return false
	}

	return path == binPath
}

// getBinPath takes a bundle name and return the path of the kite executable.
// example: fs-0.0.1.kite -> fs-0.0.1/fs/bin
func getBinPath(bundleName string) (string, error) {
	if !strings.HasSuffix(bundleName, ".kite") {
		return "", fmt.Errorf("Invalid bundle name: %s", bundleName)
	}

	fullName := strings.TrimSuffix(bundleName, ".kite")
	name, _, err := splitVersion(fullName, false)
	if err != nil {
		return "", err
	}

	return strings.Join([]string{bundleName, "bin", name}, string(os.PathSeparator)), nil
}
