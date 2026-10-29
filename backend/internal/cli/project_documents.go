package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

// These DTOs deliberately mirror the daemon boundary without importing services.
type projectDocumentActor struct {
	SessionID  string `json:"sessionId"`
	Capability string `json:"capability"`
}
type projectDocumentGrant struct {
	Epoch int64  `json:"epoch"`
	Token string `json:"token"`
}
type projectDocumentLeaseRequest struct {
	Actor projectDocumentActor `json:"actor"`
}
type projectDocumentLeaseResponse struct {
	Lease projectDocumentGrant `json:"lease"`
}
type projectDocumentWriteRequest struct {
	Actor        projectDocumentActor `json:"actor"`
	Lease        projectDocumentGrant `json:"lease"`
	Path         string               `json:"path"`
	Content      string               `json:"content"`
	ExpectedHash string               `json:"expectedHash"`
}
type projectDocumentFile struct {
	WriteReceiptID string `json:"writeReceiptId,omitempty"`
	Path           string `json:"path"`
	Content        string `json:"content"`
	Hash           string `json:"hash"`
	Size           int64  `json:"size"`
	Binary         bool   `json:"binary"`
}
type projectDocumentFileResponse struct {
	File projectDocumentFile `json:"file"`
}
type projectDocumentEntry struct {
	SourceIdentity string `json:"sourceIdentity,omitempty"`
	Backup         bool   `json:"backup,omitempty"`
	Path           string `json:"path"`
	Hash           string `json:"hash"`
	Size           int64  `json:"size"`
}
type projectDocumentAdoption struct {
	SourceRootIdentity string                 `json:"sourceRootIdentity"`
	SourceSessionID    string                 `json:"sourceSessionId"`
	Files              []projectDocumentEntry `json:"files"`
	ManifestHash       string                 `json:"manifestHash"`
	Complete           bool                   `json:"complete"`
}
type projectDocumentInfoResponse struct {
	Documents struct {
		SharedDir    string                 `json:"sharedDir"`
		DocumentPath string                 `json:"documentPath"`
		Files        []projectDocumentEntry `json:"files"`
		Lease        *struct {
			SessionID string `json:"sessionId"`
			Epoch     int64  `json:"epoch"`
			Frozen    bool   `json:"frozen"`
		} `json:"lease,omitempty"`
		Adoption *projectDocumentAdoption `json:"adoption,omitempty"`
	} `json:"documents"`
}

func newProjectDocumentsCommand(ctx *commandContext) *cobra.Command {
	var project string
	parent := &cobra.Command{Use: "documents", Short: "Read and maintain shared orchestrator documents"}
	parent.PersistentFlags().StringVarP(&project, "project", "p", "", "Project id (defaults to the current AO project)")
	for _, action := range []string{"info", "read", "write", "adopt", "download"} {
		var asJSON bool
		var inputFile, expected string
		use := action
		count := 1
		switch action {
		case "info":
			count = 0
		case "adopt":
			use += " <source-session>"
		default:
			use += " <path>"
		}
		cmd := &cobra.Command{Use: use, Short: map[string]string{"info": "Locate the shared folder and list its files", "read": "Read bounded document text and its hash with --json", "write": "Write UTF-8 text as the current orchestrator", "adopt": "Copy the approved legacy folders once", "download": "Stream a shared file to stdout"}[action], Args: exactArgs(count)}
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			if action != "info" && action != "adopt" {
				p := args[0]
				if p == "" || p == "." || p == ".." || path.Clean(p) != p || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "../") || strings.ContainsAny(p, "\\\x00") || strings.IndexFunc(p, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
					return usageError{errors.New("document path must be a confined relative path")}
				}
			}
			var actor projectDocumentActor
			var content []byte
			if action == "write" || action == "adopt" {
				if action == "write" && (expected == "" || inputFile == "") {
					return usageError{errors.New("--file and --expected-hash are required; use missing to create a new document")}
				}
				actor = projectDocumentActor{SessionID: strings.TrimSpace(os.Getenv("AO_SESSION_ID")), Capability: strings.TrimSpace(os.Getenv("AO_BROWSER_CAPABILITY"))}
				if actor.SessionID == "" || actor.Capability == "" {
					return usageError{errors.New("document writes require AO_SESSION_ID and AO_BROWSER_CAPABILITY from the owning orchestrator")}
				}
				if action == "write" {
					f, err := os.Open(inputFile)
					if err != nil {
						return err
					}
					content, err = io.ReadAll(io.LimitReader(f, (2<<20)+1))
					_ = f.Close()
					if err != nil {
						return err
					}
					if len(content) > 2<<20 {
						return usageError{errors.New("document exceeds the 2 MiB write limit")}
					}
					if !utf8.Valid(content) {
						return usageError{errors.New("document draft must contain valid UTF-8")}
					}
				}
			}
			p, err := ctx.resolveSpawnProject(cmd.Context(), project)
			if err != nil {
				return err
			}
			base := "projects/" + url.PathEscape(p.ID) + "/documents"
			switch action {
			case "info":
				var res projectDocumentInfoResponse
				if err := ctx.getJSON(cmd.Context(), base, &res); err != nil {
					return err
				}
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), res)
				}
				if _, err = fmt.Fprintf(cmd.OutOrStdout(), "Shared folder: %s\nHandoff: %s\n", res.Documents.SharedDir, res.Documents.DocumentPath); err != nil {
					return err
				}
				for _, f := range res.Documents.Files {
					link := "ao://projects/" + url.PathEscape(p.ID) + "/shared/" + encodeDocumentPath(f.Path)
					if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s (%d bytes) %s\n", f.Path, f.Size, link); err != nil {
						return err
					}
				}
				return nil
			case "read":
				var res projectDocumentFileResponse
				if err := ctx.getJSON(cmd.Context(), base+"/file?path="+url.QueryEscape(args[0]), &res); err != nil {
					return err
				}
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), res)
				}
				if res.File.Binary {
					return errors.New("binary document; use download to preserve its bytes")
				}
				_, err = io.WriteString(cmd.OutOrStdout(), res.File.Content)
				return err
			case "download":
				return ctx.doJSON(cmd.Context(), http.MethodGet, base+"/files/"+encodeDocumentPath(args[0])+"?download=1", nil, cmd.OutOrStdout())
			default:
				var grant projectDocumentLeaseResponse
				if err := ctx.postJSON(cmd.Context(), base+"/lease", projectDocumentLeaseRequest{Actor: actor}, &grant); err != nil {
					return err
				}
				if action == "adopt" {
					var res struct {
						Adoption projectDocumentAdoption `json:"adoption"`
					}
					in := struct {
						Actor           projectDocumentActor `json:"actor"`
						Lease           projectDocumentGrant `json:"lease"`
						SourceSessionID string               `json:"sourceSessionId"`
					}{actor, grant.Lease, args[0]}
					if err := ctx.postJSON(cmd.Context(), base+"/adopt", in, &res); err != nil {
						return err
					}
					return writeJSON(cmd.OutOrStdout(), res)
				}
				var res projectDocumentFileResponse
				if err := ctx.putJSON(cmd.Context(), base+"/file", projectDocumentWriteRequest{Actor: actor, Lease: grant.Lease, Path: args[0], Content: string(content), ExpectedHash: expected}, &res); err != nil {
					return err
				}
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), res)
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s sha256:%s\n", res.File.Path, res.File.Hash)
				return err
			}
		}
		if action != "download" {
			cmd.Flags().BoolVar(&asJSON, "json", false, "Output metadata as JSON")
		}
		if action == "write" {
			cmd.Flags().StringVar(&inputFile, "file", "", "Local UTF-8 draft to write")
			cmd.Flags().StringVar(&expected, "expected-hash", "", "Current SHA-256, or missing for creation")
		}
		parent.AddCommand(cmd)
	}
	return parent
}
func encodeDocumentPath(p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
