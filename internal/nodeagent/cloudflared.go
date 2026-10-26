package nodeagent

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MinCloudflared is the oldest cloudflared that no longer trusts the eMudhra
// intermediates whose keys leaked in October 2026.
const MinCloudflared = "2026.10.3"

var cloudflaredVersionRe = regexp.MustCompile(`version (\d+\.\d+\.\d+)`)

// cloudflaredVersion pulls the version out of `cloudflared --version`; "" when
// it has none, as in development builds.
func cloudflaredVersion(out string) string {
	if m := cloudflaredVersionRe.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// cloudflaredTooOld reports whether a known version is below MinCloudflared.
func cloudflaredTooOld(v string) bool {
	if v == "" {
		return false
	}
	have, want := strings.Split(v, "."), strings.Split(MinCloudflared, ".")
	for i := range want {
		h, _ := strconv.Atoi(have[i])
		w, _ := strconv.Atoi(want[i])
		if h != w {
			return h < w
		}
	}
	return false
}

func runCloudflaredVersion(ctx context.Context, bin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	return string(out), err
}
