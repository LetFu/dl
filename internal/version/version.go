// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// The version package permits running a specific version of Go.
package version

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func init() {
	http.DefaultTransport = &userAgentTransport{http.DefaultTransport}
}

// Run runs the "go" tool of the provided Go version.
func Run(version string) {
	log.SetFlags(0)

	root, err := goroot(version)
	if err != nil {
		log.Fatalf("%s: %v", version, err)
	}

	if len(os.Args) == 2 && os.Args[1] == "download" {
		if err := install(root, version); err != nil {
			log.Fatalf("%s: download failed: %v", version, err)
		}
		os.Exit(0)
	}

	if _, err := os.Stat(filepath.Join(root, unpackedOkay)); err != nil {
		log.Fatalf("%s: not downloaded. Run '%s download' to install to %v", version, version, root)
	}

	runGo(root)
}

func runGo(root string) {
	gobin := filepath.Join(root, "bin", "go"+exe())
	cmd := exec.Command(gobin, os.Args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	newPath := filepath.Join(root, "bin")
	if p := os.Getenv("PATH"); p != "" {
		newPath += string(filepath.ListSeparator) + p
	}
	cmd.Env = dedupEnv(caseInsensitiveEnv, append(os.Environ(), "GOROOT="+root, "PATH="+newPath))

	handleSignals()

	err := cmd.Run()
	if eerr, ok := err.(*exec.ExitError); ok {
		os.Exit(eerr.ExitCode())
	} else if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func fmtSize(size int64) string {
	const (
		byte_unit = 1 << (10 * iota)
		kilobyte_unit
		megabyte_unit
	)

	unit := "B"
	value := float64(size)

	switch {
	case size >= megabyte_unit:
		unit = "MB"
		value = value / megabyte_unit
	case size >= kilobyte_unit:
		unit = "KB"
		value = value / kilobyte_unit
	}
	formatted := strings.TrimSuffix(strconv.FormatFloat(value, 'f', 1, 64), ".0")
	return fmt.Sprintf("%s %s", formatted, unit)
}

// install installs a version of Go to the named target directory, creating the
// directory as needed.
func install(targetDir, version string) error {
	if _, err := os.Stat(filepath.Join(targetDir, unpackedOkay)); err == nil {
		log.Printf("%s: already downloaded in %v", version, targetDir)
		return nil
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}
	goURL := versionArchiveURL(version)
	res, err := http.Head(goURL)
	if err != nil {
		return err
	}
	if res.StatusCode == http.StatusNotFound {
		return fmt.Errorf("no binary release of %v for %v/%v at %v", version, getOS(), runtime.GOARCH, goURL)
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %v checking size of %v", http.StatusText(res.StatusCode), goURL)
	}
	base := path.Base(goURL)
	archiveFile := filepath.Join(targetDir, base)
	if fi, err := os.Stat(archiveFile); err != nil || fi.Size() != res.ContentLength {
		if err != nil && !os.IsNotExist(err) {
			// Something weird. Don't try to download.
			return err
		}
		if err := copyFromURL(archiveFile, goURL); err != nil {
			return fmt.Errorf("error downloading %v: %v", goURL, err)
		}
		fi, err = os.Stat(archiveFile)
		if err != nil {
			return err
		}
		if fi.Size() != res.ContentLength {
			return fmt.Errorf("downloaded file %s size %v doesn't match server size %v", archiveFile, fi.Size(), res.ContentLength)
		}
	}
	wantSHA, err := slurpURLToString(goURL + ".sha256")
	if err != nil {
		return err
	}
	if err := verifySHA256(archiveFile, strings.TrimSpace(wantSHA)); err != nil {
		return fmt.Errorf("error verifying SHA256 of %v: %v", archiveFile, err)
	}
	log.Printf("Unpacking %v ...", archiveFile)
	if err := unpackArchive(targetDir, archiveFile); err != nil {
		return fmt.Errorf("extracting archive %v: %v", archiveFile, err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, unpackedOkay), nil, 0644); err != nil {
		return err
	}
	log.Printf("Success. You may now run '%v'", version)
	return nil
}

// unpackArchive unpacks the provided archive zip or tar.gz file to targetDir,
// removing the "go/" prefix from file entries.
func unpackArchive(targetDir, archiveFile string) error {
	switch {
	case strings.HasSuffix(archiveFile, ".zip"):
		return unpackZip(targetDir, archiveFile)
	case strings.HasSuffix(archiveFile, ".tar.gz"):
		return unpackTarGz(targetDir, archiveFile)
	default:
		return errors.New("unsupported archive file")
	}
}

// unpackTarGz is the tar.gz implementation of unpackArchive.
func unpackTarGz(targetDir, archiveFile string) error {
	r, err := os.Open(archiveFile)
	if err != nil {
		return err
	}
	defer r.Close()
	madeDir := map[string]bool{}
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		f, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if !validRelPath(f.Name) {
			return fmt.Errorf("tar file contained invalid name %q", f.Name)
		}
		rel := filepath.FromSlash(strings.TrimPrefix(f.Name, "go/"))
		abs := filepath.Join(targetDir, rel)

		fi := f.FileInfo()
		mode := fi.Mode()
		switch {
		case mode.IsRegular():
			// Make the directory. This is redundant because it should
			// already be made by a directory entry in the tar
			// beforehand. Thus, don't check for errors; the next
			// write will fail with the same error.
			dir := filepath.Dir(abs)
			if !madeDir[dir] {
				if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
					return err
				}
				madeDir[dir] = true
			}
			wf, err := os.OpenFile(abs, os.O_RDWR|os.O_CREATE|os.O_TRUNC, mode.Perm())
			if err != nil {
				return err
			}
			n, err := io.Copy(wf, tr)
			if closeErr := wf.Close(); closeErr != nil && err == nil {
				err = closeErr
			}
			if err != nil {
				return fmt.Errorf("error writing to %s: %v", abs, err)
			}
			if n != f.Size {
				return fmt.Errorf("only wrote %d bytes to %s; expected %d", n, abs, f.Size)
			}
			if !f.ModTime.IsZero() {
				if err := os.Chtimes(abs, f.ModTime, f.ModTime); err != nil {
					// benign error. Gerrit doesn't even set the
					// modtime in these, and we don't end up relying
					// on it anywhere (the gomote push command relies
					// on digests only), so this is a little pointless
					// for now.
					log.Printf("error changing modtime: %v", err)
				}
			}
		case mode.IsDir():
			if err := os.MkdirAll(abs, 0755); err != nil {
				return err
			}
			madeDir[abs] = true
		default:
			return fmt.Errorf("tar file entry %s contained unsupported file type %v", f.Name, mode)
		}
	}
	return nil
}

// unpackZip is the zip implementation of unpackArchive.
func unpackZip(targetDir, archiveFile string) error {
	zr, err := zip.OpenReader(archiveFile)
	if err != nil {
		return err
	}
	defer zr.Close()

	for _, f := range zr.File {
		name := strings.TrimPrefix(f.Name, "go/")

		outpath := filepath.Join(targetDir, name)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(outpath, 0755); err != nil {
				return err
			}
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}

		// File
		if err := os.MkdirAll(filepath.Dir(outpath), 0755); err != nil {
			return err
		}
		out, err := os.OpenFile(outpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		if err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
	}
	return nil
}

// verifySHA256 reports whether the named file has contents with
// SHA-256 of the given wantHex value.
func verifySHA256(file, wantHex string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return err
	}
	if fmt.Sprintf("%x", hash.Sum(nil)) != wantHex {
		return fmt.Errorf("%s corrupt? does not have expected SHA-256 of %v", file, wantHex)
	}
	return nil
}

// slurpURLToString downloads the given URL and returns it as a string.
func slurpURLToString(url_ string) (string, error) {
	res, err := http.Get(url_)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %v", url_, res.Status)
	}
	slurp, err := io.ReadAll(res.Body)
	if err != nil {
		return "", fmt.Errorf("reading %s: %v", url_, err)
	}
	return string(slurp), nil
}

// copyFromURL downloads srcURL to dstFile.
func copyFromURL(dstFile, srcURL string) (err error) {
	f, err := os.Create(dstFile)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(dstFile)
		}
	}()
	c := &http.Client{
		Transport: &userAgentTransport{&http.Transport{
			// It's already compressed. Prefer accurate ContentLength.
			// (Not that GCS would try to compress it, though)
			DisableCompression: true,
			DisableKeepAlives:  true,
			Proxy:              http.ProxyFromEnvironment,
		}},
	}
	res, err := c.Get(srcURL)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return errors.New(res.Status)
	}
	pw := &progressWriter{w: f, total: res.ContentLength, output: os.Stderr}
	n, err := io.Copy(pw, res.Body)
	if err != nil {
		return err
	}
	if res.ContentLength != -1 && res.ContentLength != n {
		return fmt.Errorf("copied %v bytes; expected %v", n, res.ContentLength)
	}
	pw.update() // 100%
	return f.Close()
}

type progressWriter struct {
	w         io.Writer
	n         int64
	total     int64
	last      time.Time
	formatted bool
	output    io.Writer
}

func (p *progressWriter) update() {
	end := " ..."
	if p.n == p.total {
		end = ""
	}
	if p.formatted {
		fmt.Fprintf(p.output, "Downloaded %5.1f%% (%s / %s)%s\n",
			(100.0*float64(p.n))/float64(p.total),
			fmtSize(p.n), fmtSize(p.total), end)
	} else {
		fmt.Fprintf(p.output, "Downloaded %5.1f%% (%*d / %d bytes)%s\n",
			(100.0*float64(p.n))/float64(p.total),
			ndigits(p.total), p.n, p.total, end)
	}
}

func ndigits(i int64) int {
	var n int
	for ; i != 0; i /= 10 {
		n++
	}
	return n
}

func (p *progressWriter) Write(buf []byte) (n int, err error) {
	n, err = p.w.Write(buf)
	p.n += int64(n)
	if now := time.Now(); now.Unix() != p.last.Unix() {
		p.update()
		p.last = now
	}
	return
}

// getOS returns runtime.GOOS. It exists as a function just for lazy
// testing of the Windows zip path when running on Linux/Darwin.
func getOS() string {
	return runtime.GOOS
}

// versionArchiveURL returns the zip or tar.gz URL of the given Go version.
func versionArchiveURL(version string) string {
	goos := getOS()

	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	arch := runtime.GOARCH
	if goos == "linux" && runtime.GOARCH == "arm" {
		arch = "armv6l"
	}
	return "https://dl.google.com/go/" + version + "." + goos + "-" + arch + ext
}

const caseInsensitiveEnv = runtime.GOOS == "windows"

// unpackedOkay is a sentinel zero-byte file to indicate that the Go
// version was downloaded and unpacked successfully.
const unpackedOkay = ".unpacked-success"

func exe() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func goroot(version string) (string, error) {
	if dir := os.Getenv("GOPATH"); dir != "" {
		return filepath.Join(dir, "sdk", version), nil
	}

	home, err := homedir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %v", err)
	}
	return filepath.Join(home, "sdk", version), nil
}

func homedir() (string, error) {
	// This could be replaced with os.UserHomeDir, but it was introduced too
	// recently, and we want this to work with go as packaged by Linux
	// distributions. Note that user.Current is not enough as it does not
	// prioritize $HOME. See also Issue 26463.
	switch getOS() {
	case "plan9":
		return "", fmt.Errorf("%q not yet supported", runtime.GOOS)
	case "windows":
		if dir := os.Getenv("USERPROFILE"); dir != "" {
			return dir, nil
		}
		return "", errors.New("can't find user home directory; %USERPROFILE% is empty")
	default:
		if dir := os.Getenv("HOME"); dir != "" {
			return dir, nil
		}
		if u, err := user.Current(); err == nil && u.HomeDir != "" {
			return u.HomeDir, nil
		}
		return "", errors.New("can't find user home directory; $HOME is empty")
	}
}

func validRelPath(p string) bool {
	if p == "" || strings.Contains(p, `\`) || strings.HasPrefix(p, "/") || strings.Contains(p, "../") {
		return false
	}
	return true
}

type userAgentTransport struct {
	rt http.RoundTripper
}

func (uat userAgentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	version := runtime.Version()
	if strings.Contains(version, "devel") {
		// Strip the SHA hash and date. We don't want spaces or other tokens (see RFC2616 14.43)
		version = "devel"
	}
	r.Header.Set("User-Agent", "golang-x-build-version/"+version)
	return uat.rt.RoundTrip(r)
}

// dedupEnv returns a copy of env with any duplicates removed, in favor of
// later values.
// Items are expected to be on the normal environment "key=value" form.
// If caseInsensitive is true, the case of keys is ignored.
//
// This function is unnecessary when the binary is
// built with Go 1.9+, but keep it around for now until Go 1.8
// is no longer seen in the wild in common distros.
//
// This is copied verbatim from github.com/LetFu/x/build/envutil.Dedup at CL 10301
// (commit a91ae26).
func dedupEnv(caseInsensitive bool, env []string) []string {
	out := make([]string, 0, len(env))
	saw := map[string]int{} // to index in the array
	for _, kv := range env {
		eq := strings.Index(kv, "=")
		if eq < 1 {
			out = append(out, kv)
			continue
		}
		k := kv[:eq]
		if caseInsensitive {
			k = strings.ToLower(k)
		}
		if dupIdx, isDup := saw[k]; isDup {
			out[dupIdx] = kv
		} else {
			saw[k] = len(out)
			out = append(out, kv)
		}
	}
	return out
}

func handleSignals() {
	// Ensure that signals intended for the child process are not handled by
	// this process' runtime (e.g. SIGQUIT). See issue #36976.
	signal.Notify(make(chan os.Signal), signalsToIgnore...)
}
