//go:build linux

package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/spf13/cobra"
)

func startOptions(c *cobra.Command, args []string) (session.StartOptions, error) {
	f := c.Flags()
	mode, _ := f.GetString("mode")
	count, _ := f.GetInt("count")
	domain, _ := f.GetString("domain")
	difficulty, _ := f.GetString("difficulty")
	tag, _ := f.GetString("tag")
	seed, _ := f.GetUint64("seed")
	retry, _ := f.GetString("retry")
	failed, _ := f.GetBool("failed")
	newSeed, _ := f.GetBool("new-seed")
	o := session.StartOptions{Mode: mode, Count: count, Domain: domain, Difficulty: difficulty, Tag: tag, Seed: seed, RetrySessionID: retry, NewSeed: newSeed}
	if retry != "" {
		if !failed || len(args) > 0 || f.Changed("count") || f.Changed("mode") || domain != "" || difficulty != "" || tag != "" || f.Changed("seed") {
			return o, fail(2, "재도전은 start --retry <session-id> --failed [--new-seed]로 요청하세요.")
		}
		o.Mode = model.ModeRetry
		return o, nil
	}
	if failed || newSeed {
		return o, fail(2, "--failed와 --new-seed는 --retry와 함께 사용하세요.")
	}
	switch mode {
	case model.ModeMock:
		if domain != "" || difficulty != "" || tag != "" || f.Changed("count") {
			return o, fail(2, "모의시험은 고정 구성입니다. 필터와 --count는 --mode practice에서 사용하세요.")
		}
		o.MockID = "mock-01"
		if len(args) > 0 {
			o.MockID = args[0]
		}
	case model.ModePractice:
		if len(args) > 0 {
			return o, fail(2, "훈련에는 모의시험 ID를 지정할 수 없습니다.")
		}
		if !f.Changed("count") {
			o.Count = 5
		}
		if o.Count < 5 || o.Count > 20 {
			return o, fail(2, "훈련 문제 수는 5~20개입니다.")
		}
	default:
		return o, fail(2, "--mode는 mock 또는 practice를 지정하세요.")
	}
	return o, nil
}

func validateRequest(c *cobra.Command, args []string) error {
	switch c.Name() {
	case "start":
		_, err := startOptions(c, args)
		return err
	case "grade":
		a, _ := c.Flags().GetBool("retry")
		b, _ := c.Flags().GetBool("resubmit")
		if a && b {
			return fail(2, "--retry와 --resubmit 중 하나만 지정하세요.")
		}
	case "mark":
		a, _ := c.Flags().GetBool("review")
		b, _ := c.Flags().GetBool("clear")
		if a == b {
			return fail(2, "--review 또는 --clear 중 하나를 지정하세요.")
		}
	case "review":
		solution, _ := c.Flags().GetBool("solution")
		pid, _ := c.Flags().GetString("problem")
		if solution && pid == "" {
			return fail(2, "해설은 review --problem Q4 --solution처럼 문제를 지정해 여세요.")
		}
	case "reset":
		cluster, _ := c.Flags().GetBool("cluster")
		pid, _ := c.Flags().GetString("problem")
		recover, _ := c.Flags().GetBool("recover")
		if recover && (cluster || pid != "") {
			return fail(2, "--recover는 단독으로 사용하세요.")
		}
		if cluster && pid != "" {
			return fail(2, "--cluster와 --problem을 함께 지정할 수 없습니다.")
		}
	}
	return nil
}

func startCommand(d *Dependencies) *cobra.Command {
	c := &cobra.Command{Use: "start [mock-id]", Short: "환경과 문제를 준비하고 확인 후 시작", Args: cobra.MaximumNArgs(1)}
	c.Flags().String("mode", "mock", "mock 또는 practice")
	c.Flags().Int("count", 0, "훈련 문제 수 (5~20, 기본 5)")
	c.Flags().String("domain", "", "훈련 domain 필터")
	c.Flags().String("difficulty", "", "훈련 difficulty 필터")
	c.Flags().String("tag", "", "훈련 tag 필터")
	c.Flags().Uint64("seed", 0, "문제 선택 seed")
	c.Flags().String("retry", "", "오답 원본 session-id")
	c.Flags().Bool("failed", false, "오답만 재도전")
	c.Flags().Bool("new-seed", false, "검증 범위에서 변형 재도전")
	c.RunE = func(c *cobra.Command, args []string) error {
		opts, err := startOptions(c, args)
		if err != nil {
			return err
		}
		if d.Session == nil || d.Store == nil {
			return publicError(errors.New("missing service"))
		}
		// Resume a prepared session after an interrupted confirmation. Explicit
		// arguments request a new session and retain the service's active guard.
		var view session.SessionView
		if len(args) == 0 && c.LocalNonPersistentFlags().NFlag() == 0 {
			if id, err := d.Store.CurrentSessionID(c.Context()); err == nil {
				st, err := d.Session.Status(c.Context(), id, d.Now())
				if err != nil {
					return publicError(err)
				}
				if st.Session.State == model.StatePreparing {
					view = st.Session
				}
			} else if !errors.Is(err, store.ErrNotFound) {
				return publicError(err)
			}
		}
		if view.ID == "" {
			fmt.Fprintln(c.ErrOrStderr(), "환경과 문제를 준비하고 있습니다. 중단 후에는 ckad status로 확인하세요.")
			var report session.Report
			view, report, err = d.Session.PrepareWithReport(c.Context(), opts)
			for _, x := range report.Excluded {
				fmt.Fprintf(c.OutOrStdout(), "제외: %s — 검증 또는 환경 capability를 확인하세요.\n", x.ProblemID)
			}
			if err != nil {
				return publicError(err)
			}
		}
		fmt.Fprintf(c.OutOrStdout(), "준비 완료: %s | %d문제 | 제한 %s | 환경 %s\n시작하시겠습니까? [yes/no]: ", view.ID, len(view.Problems), view.DurationLimit, view.EnvironmentID)
		// A blocked terminal read must not hold up SIGINT/SIGTERM. The reader
		// belongs to the caller; cancellation never closes a shared stdin.
		type answerLine struct {
			text string
			err  error
		}
		answerReady := make(chan answerLine, 1)
		input := c.InOrStdin()
		go func() { line, err := bufio.NewReader(input).ReadString('\n'); answerReady <- answerLine{line, err} }()
		var line string
		var readErr error
		select {
		case <-c.Context().Done():
			return publicError(c.Context().Err())
		case answer := <-answerReady:
			line, readErr = answer.text, answer.err
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return publicError(readErr)
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer != "yes" && answer != "y" && answer != "예" {
			fmt.Fprintln(c.OutOrStdout(), "준비 상태를 보존했습니다. ckad start로 시작 확인을 다시 할 수 있습니다.")
			return nil
		}
		if err := d.Session.Start(c.Context(), view.ID, d.Now()); err != nil {
			return publicError(err)
		}
		fmt.Fprintf(c.OutOrStdout(), "학습 시작: %s\nckad status로 목록을 보고 ckad show Q1로 문제를 여세요.\n", view.ID)
		return nil
	}
	return c
}
