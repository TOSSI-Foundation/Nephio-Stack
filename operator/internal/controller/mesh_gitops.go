/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The in-cluster Gitea the deploy repos live in (the same one Porch + Config Sync use).
const giteaBase = "http://gitea-http.gitea.svc.cluster.local:3000"

// deployRepoFor maps a cluster name to the Gitea deploy repo whose RootSync that cluster's Config Sync
// watches: the control plane runs on the mgmt cluster ("mgmt" repo); an edge runs on its own cluster
// ("cluster-<edge>" repo).
func deployRepoFor(cluster string) string {
	if cluster == "" || cluster == "mgmt" {
		return "mgmt"
	}
	return "cluster-" + cluster
}

// deliverViaGit UPSERTs one KRM file into a Gitea deploy repo (create or update), so the GitOps agent
// (Config Sync) reconciles it onto the target cluster. This is the pure-Nephio delivery path: the domain
// controller PRODUCES the KRM, Git is the source of truth, and Config Sync makes it running reality —
// instead of the controller touching the target cluster directly.
func (r *MeshLinkReconciler) deliverViaGit(ctx context.Context, repo, path, yaml string) error {
	return gitPut(ctx, r.Client, repo, path, yaml)
}

// gitPut UPSERTs one KRM file into a Gitea deploy repo — the shared GitOps-delivery primitive used by both
// the MeshLink controller (mesh secrets) and the EdgeSite controller (a self-contained core's slice/SIMs).
// The domain controller PRODUCES the KRM, Git is the source of truth, and that repo's Config Sync makes it
// running reality on the target cluster — never a direct client-go write to the target.
func gitPut(ctx context.Context, c client.Client, repo, path, yaml string) error {
	// Gitea credentials from the in-cluster gitea-auth secret (username/password), same as Porch uses.
	var sec corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "gitea-auth"}, &sec); err != nil {
		return fmt.Errorf("read gitea-auth: %w", err)
	}
	user := string(sec.Data["username"])
	pass := string(sec.Data["password"])
	api := fmt.Sprintf("%s/api/v1/repos/%s/%s/contents/%s", giteaBase, user, repo, path)

	// look up the current file SHA (needed to UPDATE; absent => CREATE).
	sha := ""
	if req, _ := http.NewRequestWithContext(ctx, http.MethodGet, api+"?ref=main", nil); req != nil {
		req.SetBasicAuth(user, pass)
		if resp, err := httpc.Do(req); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var meta struct {
					Sha string `json:"sha"`
				}
				b, _ := io.ReadAll(resp.Body)
				_ = json.Unmarshal(b, &meta)
				sha = meta.Sha
			}
		}
	}

	body := map[string]interface{}{
		"message": "meshlink: reconcile clustermesh KRM " + path,
		"content": base64.StdEncoding.EncodeToString([]byte(yaml)),
		"branch":  "main",
	}
	if sha != "" {
		body["sha"] = sha // update
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, api, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.SetBasicAuth(user, pass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return fmt.Errorf("PUT %s -> HTTP %d: %s", path, resp.StatusCode, string(msg))
	}
	return nil
}

// removeFromGit deletes one file from a Gitea deploy repo so Config Sync PRUNES the resource it declared.
// Absent file (404) is success — the removal is a converged state, not an action.
func (r *MeshLinkReconciler) removeFromGit(ctx context.Context, repo, path string) error {
	var sec corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "gitea-auth"}, &sec); err != nil {
		return fmt.Errorf("read gitea-auth: %w", err)
	}
	user := string(sec.Data["username"])
	pass := string(sec.Data["password"])
	api := fmt.Sprintf("%s/api/v1/repos/%s/%s/contents/%s", giteaBase, user, repo, path)

	// the delete API needs the current file SHA; no file -> nothing to remove.
	sha := ""
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, api+"?ref=main", nil)
	req.SetBasicAuth(user, pass)
	if resp, err := httpc.Do(req); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil
		}
		if resp.StatusCode == http.StatusOK {
			var meta struct {
				Sha string `json:"sha"`
			}
			b, _ := io.ReadAll(resp.Body)
			_ = json.Unmarshal(b, &meta)
			sha = meta.Sha
		}
	}
	if sha == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]interface{}{
		"message": "meshlink: retire " + path + " (owned by the cilium chart now)",
		"sha":     sha,
		"branch":  "main",
	})
	dreq, err := http.NewRequestWithContext(ctx, http.MethodDelete, api, bytes.NewReader(body))
	if err != nil {
		return err
	}
	dreq.SetBasicAuth(user, pass)
	dreq.Header.Set("Content-Type", "application/json")
	resp, err := httpc.Do(dreq)
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return fmt.Errorf("DELETE %s -> HTTP %d: %s", path, resp.StatusCode, string(msg))
	}
	return nil
}

// secretYAML renders a corev1.Secret as a Config-Sync-appliable KRM manifest (Opaque, stringData-free —
// base64 data as k8s expects). data is name->raw bytes.
func secretYAML(name, namespace string, data map[string][]byte) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: %s\ntype: Opaque\ndata:\n", name, namespace)
	for k, v := range data {
		fmt.Fprintf(&b, "  %s: %s\n", k, base64.StdEncoding.EncodeToString(v))
	}
	return b.String()
}
