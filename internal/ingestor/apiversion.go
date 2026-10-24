package ingestor

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// knownAPIVersions is newest first. Each Slurm release serves a few of these
// (26.05 serves v0.0.42-v0.0.45), so detection picks the newest one this
// slurmrestd has.
var knownAPIVersions = []string{"v0.0.45", "v0.0.44", "v0.0.43", "v0.0.42", "v0.0.41", "v0.0.40", "v0.0.39"}

// detectAPIVersion pings slurmdb/<version>/ping from newest to oldest and
// returns the first version slurmrestd answers.
func detectAPIVersion(ctx context.Context, client *http.Client, baseURL, user, token string) (string, error) {
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	for _, version := range knownAPIVersions {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"slurmdb/"+version+"/ping", nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("X-SLURM-USER-NAME", user)
		if token != "" {
			req.Header.Set("X-SLURM-USER-TOKEN", token)
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("slurmrestd unreachable: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return version, nil
		}
		// slurmrestd checks the token before the path, so this isn't a version problem
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return "", fmt.Errorf("slurmrestd rejected the token (HTTP %d); check SLURM_API_TOKEN and SLURM_API_ACCOUNT", resp.StatusCode)
		}
	}
	return "", fmt.Errorf("slurmrestd serves none of %s; set SLURM_API_VERSION", strings.Join(knownAPIVersions, ", "))
}

// ensureAPIVersion fills in cfg.SlurmAPIVersion when it was left unset.
func (i *Ingestor) ensureAPIVersion(ctx context.Context) error {
	if i.cfg.SlurmAPIVersion != "" {
		return nil
	}
	version, err := detectAPIVersion(ctx, i.client, i.cfg.SlurmURL, i.cfg.SlurmUser, i.cfg.SlurmToken)
	if err != nil {
		return err
	}
	log.Printf("SLURM_API_VERSION not set; using %s, the newest this slurmrestd serves", version)
	i.cfg.SlurmAPIVersion = version
	return nil
}
