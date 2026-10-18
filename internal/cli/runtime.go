//go:build linux

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/spf13/cobra"
)

// Run composes the real Linux executable, lazily after parsing and validating
// the command. Read-only commands never initialize Docker or take env.lock.
// All paths are installation-local; the host kubeconfig is never consulted.
func Run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	d := Dependencies{}
	root := newRoot(&d, in, out, errOut)
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(errOut, "사용자 홈 경로를 확인할 수 없습니다.")
		return 3
	}
	state := envOr("CKAD_STATE_DIR", filepath.Join(home, ".local", "share", "junkloud"))
	bank := envOr("CKAD_BANK_DIR", "/src")
	workspace := envOr("CKAD_WORKSPACE", filepath.Join(home, "ckad"))
	compat := os.Getenv("CKAD_HOST_COMPAT")
	root.PersistentFlags().StringVar(&state, "state-dir", state, "JunKloud 전용 상태 디렉터리 (CKAD_STATE_DIR)")
	root.PersistentFlags().StringVar(&bank, "bank-dir", bank, "problems/·exams/를 포함한 은행 루트 (CKAD_BANK_DIR)")
	root.PersistentFlags().StringVar(&workspace, "workspace", workspace, "풀이 작업 디렉터리 (CKAD_WORKSPACE)")
	var st *store.Store
	var rt *author.Runtime
	var g *author.Grader
	defer func() {
		if rt != nil {
			rt.Close()
		}
		if g != nil {
			g.Close()
		}
		if st != nil {
			st.Close()
		}
	}()
	original := root.PersistentPreRunE
	root.PersistentPreRunE = func(c *cobra.Command, args []string) error {
		if err := original(c, args); err != nil {
			return err
		}
		var err error
		state, err = filepath.Abs(state)
		if err != nil {
			return publicError(err)
		}
		bank, err = filepath.Abs(bank)
		if err != nil {
			return publicError(err)
		}
		workspace, err = filepath.Abs(workspace)
		if err != nil {
			return publicError(err)
		}
		st, err = store.Open(filepath.Join(state, "sessions.db"))
		if err != nil {
			return publicError(err)
		}
		d.Store = st
		d.Session = session.NewReader(st, bank, state, workspace)
		switch c.Name() {
		case "status", "show", "mark", "review":
			return nil
		}
		hostCompat, err := environment.ParseHostCompat(compat)
		if err != nil {
			return fail(2, "CKAD_HOST_COMPAT 값을 확인하세요 (cgroupv1,no-ipv6).")
		}
		m := environment.New(state, st, environment.WithHostCompat(hostCompat))
		d.Environment = m
		if c.Name() == "validate" {
			// Refuse active learning before even preparing the platform. This
			// is a fast public preflight; Validator.Validate also checks the
			// active-session rule under its authoritative environment lock.
			id, err := m.RecordedIdentity()
			if err == nil {
				active, err := st.ActiveSessions(c.Context(), id.ID)
				if err != nil {
					return publicError(err)
				}
				if len(active) > 0 {
					return publicError(author.ErrSessionActive)
				}
			} else if !errors.Is(err, environment.ErrNoEnvironment) {
				return publicError(err)
			}
		}
		cfg := author.GraderConfig{StateDir: state}
		if c.Name() == "grade" || c.Name() == "reset" {
			// Grade must record unavailable/stale environments as ERROR runs. Use
			// the local recorded identity to compose clients without an API call;
			// Service.Grade performs live ownership and version checks after intent.
			id, err := m.RecordedIdentity()
			if err != nil {
				return publicError(err)
			}
			g, err = author.ComposeGrader(m, id, cfg)
			if err != nil {
				return publicError(err)
			}
			gradingRuntime := &author.Runtime{Identity: id, Grader: g, Config: cfg}
			d.Session = session.New(st, m, gradingRuntime, bank, state, session.WithWorkspace(workspace))
			return nil
		}
		fmt.Fprintln(c.ErrOrStderr(), "전용 Linux 학습 환경을 확인하고 있습니다.")
		id, err := author.EnsureEnvironment(c.Context(), m, state)
		if err != nil {
			return publicError(err)
		}
		rt, err = author.NewRuntime(c.Context(), m, st, id, cfg)
		if err != nil {
			return publicError(err)
		}
		d.Author = rt.Validator
		d.Session = session.New(st, m, rt, bank, state, session.WithWorkspace(workspace))
		return nil
	}
	return Execute(ctx, root, args)
}
func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
