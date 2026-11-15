// Package argocd reads actual cluster state from the Argo CD API server.
//
// The agent deliberately does not hold a Kubernetes client or a kubeconfig. Argo CD
// already has cluster credentials and already computes sync and health, so going through
// its API keeps exactly one component holding cluster-admin, and gives the agent an
// access-token-shaped credential that is easy to scope, rotate and audit.
package argocd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/drift"
	"github.com/ofirc/hello-agent/01-slack-sre-chatbot/internal/providers/httpx"
)

type Client struct {
	base    string
	token   string
	project string
	http    *http.Client
}

// New builds the client. insecureTLS skips certificate verification, which Argo CD
// needs out of the box: a default install serves its API with a self-signed
// certificate, so a correct client refuses to talk to it.
//
// This is scoped to this one provider and nothing else, and it is a demo shortcut, not
// a pattern. The real fix is to front argocd-server with a certificate the agent
// trusts, via cert-manager and an internal CA or a managed certificate on a Gateway.
// Until then the connection is still encrypted and still crosses only private VPC
// addresses, it is just unauthenticated at the server end.
func New(base, token, project string, insecureTLS bool) *Client {
	c := httpx.Default()
	if insecureTLS {
		c.Transport = httpx.InsecureTransport()
	}
	return &Client{
		base: strings.TrimRight(base, "/"), token: token, project: project,
		http: c,
	}
}

func (c *Client) Configured() bool { return c.base != "" && c.token != "" }

// applicationList mirrors only the fields we need. Argo CD's Application type is
// enormous, and decoding all of it into a struct would be a maintenance liability for no
// gain.
type applicationList struct {
	Items []struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			Destination struct {
				Namespace string `json:"namespace"`
			} `json:"destination"`
		} `json:"spec"`
		Status struct {
			Sync struct {
				Status   string `json:"status"`
				Revision string `json:"revision"`
			} `json:"sync"`
			Health struct {
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"health"`
			Summary struct {
				Images []string `json:"images"`
			} `json:"summary"`
			OperationState struct {
				Phase      string    `json:"phase"`
				Message    string    `json:"message"`
				FinishedAt time.Time `json:"finishedAt"`
			} `json:"operationState"`
		} `json:"status"`
	} `json:"items"`
}

// Applications returns one Actual per Argo CD application.
//
// Service and environment come from the labels app.kubernetes.io/name and
// app.kubernetes.io/part-of respectively, falling back to parsing the conventional
// "<service>-<env>" application name. Convention over configuration, with an explicit
// escape hatch via labels.
func (c *Client) Applications(ctx context.Context) ([]drift.Actual, error) {
	if !c.Configured() {
		return nil, fmt.Errorf("argo cd is not configured (set ARGOCD_BASE_URL and ARGOCD_TOKEN)")
	}

	q := url.Values{}
	if c.project != "" {
		q.Set("projects", c.project)
	}
	endpoint := c.base + "/api/v1/applications?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	body, err := httpx.Do(c.http, req, "argocd")
	if err != nil {
		return nil, err
	}

	var list applicationList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("argocd: decode applications: %w", err)
	}

	out := make([]drift.Actual, 0, len(list.Items))
	for _, it := range list.Items {
		svc := it.Metadata.Labels["app.kubernetes.io/name"]
		env := it.Metadata.Labels["app.kubernetes.io/part-of"]
		if svc == "" || env == "" {
			svc, env = splitAppName(it.Metadata.Name)
		}
		if svc == "" {
			continue
		}

		msg := it.Status.OperationState.Message
		if it.Status.OperationState.Phase == "Succeeded" {
			msg = "" // only surface the message when something went wrong
		}
		if msg == "" && it.Status.Health.Message != "" && it.Status.Health.Status != "Healthy" {
			msg = it.Status.Health.Message
		}

		repo, tag := imageForService(it.Status.Summary.Images, svc)
		out = append(out, drift.Actual{
			Service:     svc,
			Env:         env,
			Version:     tag,
			Image:       repo,
			App:         it.Metadata.Name,
			Namespace:   it.Spec.Destination.Namespace,
			SyncStatus:  it.Status.Sync.Status,
			Health:      it.Status.Health.Status,
			LastSyncAt:  it.Status.OperationState.FinishedAt,
			LastMessage: oneLine(msg),
		})
	}
	return out, nil
}

// Sync triggers a refresh+sync of one application. Used by drift debug to rule out
// "Argo simply has not looked recently" before blaming the rollout.
func (c *Client) Refresh(ctx context.Context, app string) error {
	if !c.Configured() {
		return fmt.Errorf("argo cd is not configured")
	}
	endpoint := fmt.Sprintf("%s/api/v1/applications/%s?refresh=normal", c.base, url.PathEscape(app))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	_, err = httpx.Do(c.http, req, "argocd")
	return err
}

// imageForService picks the image whose repository mentions the service, returning the
// repository and the tag separately. Argo reports every image in the application,
// including sidecars, so matching on the service name is what avoids reporting the
// version of a log shipper as the version of the service.
func imageForService(images []string, service string) (repo, tag string) {
	var fbRepo, fbTag string
	for _, img := range images {
		r, t := splitImage(img)
		if t == "" {
			continue
		}
		if strings.Contains(r, service) {
			return r, t
		}
		if fbTag == "" {
			fbRepo, fbTag = r, t
		}
	}
	return fbRepo, fbTag
}

// splitImage handles registry hosts carrying a port, where the last colon is the tag but
// an earlier one is not. Splitting on the first colon is the classic bug here.
func splitImage(ref string) (repo, tag string) {
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at] // digest-pinned, no usable tag
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i+1:], "/") {
		return ref, ""
	}
	return ref[:i], ref[i+1:]
}

func splitAppName(name string) (service, env string) {
	i := strings.LastIndex(name, "-")
	if i <= 0 {
		return name, ""
	}
	return name[:i], name[i+1:]
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 240 {
		s = s[:240] + "..."
	}
	return s
}
