// Adapted from zbxctl (github.com/mwtrigg/zbxctl, internal/cli/upgrade.go): port fixes both ways.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwtrigg/awxctl/internal/output"
	"github.com/mwtrigg/awxctl/internal/version"
)

var githubReleaseAPI = "https://api.github.com/repos/mwtrigg/awxctl/releases/latest"

type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// errNoRelease means GitHub has no latest (non-prerelease) release for the project.
var errNoRelease = errors.New("no release found")

var upgradeCheckOnly bool

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Update awxctl to the latest release",
	Long: `Installs the latest release over the running binary:

  awxctl upgrade            # do it
  awxctl upgrade --check    # only say whether there is a newer release

It asks nothing and needs no --apply: saying "upgrade" is the decision. What makes
that safe is checked every time: the release's checksums must be signed with the
key compiled into this binary, the archive must match them, an older version is
never installed, and the new binary is started once and the old one put back if
it does not run. Sends no credentials.`,
	RunE: runUpgrade,
}

// installRelease installs a release over the running binary (a var so that a
// test can stand in for the download).
var installRelease = installLatest

func init() {
	rootCmd.AddCommand(upgradeCmd)
	upgradeCmd.Flags().BoolVar(&upgradeCheckOnly, "check", false, "check for updates without installing")
	upgradeCmd.Flags().BoolVar(&upgradeKeepOld, "keep-old", false, "keep the previous binary next to the new one as awxctl.old (otherwise it is removed once the new one has started)")
}

func runUpgrade(_ *cobra.Command, _ []string) error {
	outFmt := output.Format(flagOutput)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	current := version.Version
	rel, err := fetchLatestRelease(ctx)
	if errors.Is(err, errNoRelease) {
		// GitHub's "latest release" ignores prereleases, so with only a release candidate
		// published there is nothing to find. That is not a failure.
		const note = "no final release has been published yet (prereleases are not considered), so there is nothing to upgrade to"
		if outFmt == output.FormatTable {
			fmt.Printf("%s (%s)\n", note, current)
			return nil
		}
		return output.Print(outFmt, []output.Column{{Key: "current_version", Header: "CURRENT"}, {Key: "latest_version", Header: "LATEST"}, {Key: "update_available", Header: "UPDATE_AVAILABLE"}, {Key: "note", Header: "NOTE"}},
			[]map[string]any{{"current_version": current, "latest_version": "", "update_available": false, "note": note}})
	}
	if err != nil {
		return cliPrintErr(outFmt, "failed to fetch release info: "+err.Error(), output.ExitError)
	}

	latest := rel.TagName
	// an update is a different version that is not older than the one running
	updateAvailable := stripV(current) != stripV(latest) && !olderRelease(current, latest)

	// --dry-run means here what it means everywhere: look, change nothing.
	if upgradeCheckOnly || flagDryRun || !updateAvailable {
		if outFmt == output.FormatTable {
			if updateAvailable {
				fmt.Printf("update available: %s → %s\n", current, latest)
			} else {
				fmt.Printf("up to date (%s)\n", current)
			}
			return nil
		}
		cols := []output.Column{
			{Key: "current_version", Header: "CURRENT"},
			{Key: "latest_version", Header: "LATEST"},
			{Key: "update_available", Header: "UPDATE_AVAILABLE"},
		}
		row := map[string]any{
			"current_version":  current,
			"latest_version":   latest,
			"update_available": updateAvailable,
		}
		return output.Print(outFmt, cols, []map[string]any{row})
	}

	fmt.Fprintf(os.Stderr, "upgrading %s → %s\n", current, latest)

	if olderRelease(current, latest) {
		return cliPrintErr(outFmt, fmt.Sprintf("the latest release (%s) is older than the version running (%s); not downgrading", latest, current), output.ExitError)
	}
	fmt.Fprintf(os.Stderr, "downloading and verifying %s...\n", archiveName(latest, runtime.GOOS, runtime.GOARCH))
	if err := installRelease(ctx, rel); err != nil {
		return cliPrintErr(outFmt, "upgrade failed: "+err.Error(), output.ExitError)
	}

	if outFmt == output.FormatTable {
		fmt.Printf("upgraded to %s\n", latest)
		return nil
	}
	cols := []output.Column{
		{Key: "current_version", Header: "PREVIOUS"},
		{Key: "latest_version", Header: "INSTALLED"},
		{Key: "updated", Header: "UPDATED"},
	}
	row := map[string]any{
		"current_version": current,
		"latest_version":  latest,
		"updated":         true,
	}
	return output.Print(outFmt, cols, []map[string]any{row})
}

func fetchLatestRelease(ctx context.Context) (*githubRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubReleaseAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "awxctl/"+version.Version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, errNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %s", resp.Status)
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

func downloadAsset(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "awxctl/"+version.Version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %s", resp.Status)
	}

	_, err = io.Copy(w, resp.Body)
	return err
}

func stripV(v string) string {
	return strings.TrimPrefix(v, "v")
}
