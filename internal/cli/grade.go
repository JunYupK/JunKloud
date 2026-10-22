//go:build linux

package cli

import (
	"errors"
	"fmt"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/spf13/cobra"
)

func gradeCommand(d *Dependencies) *cobra.Command {
	c := &cobra.Command{Use: "grade", Short: "전체 제출 또는 훈련 문제 채점", Args: cobra.NoArgs}
	c.Flags().String("problem", "", "훈련 문제 Q-number 또는 problem-id")
	c.Flags().Bool("retry", false, "수정하지 않은 오류 제출의 기술적 채점 재시도")
	c.Flags().Bool("resubmit", false, "수정 후 새 제출 기록")
	c.RunE = func(c *cobra.Command, _ []string) error {
		id, err := current(c, d)
		if err != nil {
			return err
		}
		pid, _ := c.Flags().GetString("problem")
		if pid != "" {
			p, err := resolve(c, d, id, pid)
			if err != nil {
				return err
			}
			pid = p.ID
		}
		retry, _ := c.Flags().GetBool("retry")
		resubmit, _ := c.Flags().GetBool("resubmit")
		fmt.Fprintln(c.ErrOrStderr(), "채점 중에는 답안 수정을 멈추세요. 중단 후 ckad status로 복구 방법을 확인하세요.")
		run, err := d.Session.Grade(c.Context(), id, session.GradeOptions{ProblemID: pid, Retry: retry, Resubmit: resubmit, At: d.Now()})
		existing := errors.Is(err, session.ErrAlreadySubmitted)
		if err != nil && !existing {
			return publicError(err)
		}
		rv, err := d.Session.Review(c.Context(), id, pid, false)
		if err != nil {
			return publicError(err)
		}
		printRun(c, run, rv.Feedback)
		st, err := d.Session.Status(c.Context(), id, d.Now())
		if err != nil {
			return publicError(err)
		}
		if st.Session.Mode == model.ModeMock && pid == "" {
			printReference(c, run)
			if len(rv.ReviewRuns) > 0 {
				fmt.Fprintln(c.OutOrStdout(), "복습 제출 기록입니다. 최초 모의시험 점수는 ckad review에서 별도로 확인하세요.")
			}
		}
		printSubmissionTimes(c, rv.Submissions)
		printSubmissionTimes(c, rv.ReviewSubmissions)
		if existing {
			fmt.Fprintln(c.OutOrStdout(), "이미 제출했습니다. 기존 결과입니다. 기술적 오류는 ckad grade --retry, 수정 후에는 --resubmit, 결과는 ckad review로 확인하세요.")
			return nil
		}
		if !run.Evaluation.Complete || run.Evaluation.Score == nil {
			return fail(3, "채점을 확정할 수 없습니다. ckad status를 확인한 뒤 ckad grade --retry 또는 수정 후 --resubmit을 사용하세요.")
		}
		if *run.Evaluation.Score < run.Evaluation.MaxScore {
			return fail(1, "")
		}
		return nil
	}
	return c
}

func printRun(c *cobra.Command, run model.GradingRun, feedback map[string]string) {
	fmt.Fprintf(c.OutOrStdout(), "채점 실행: %s | 관찰: %s | 채점 소요: %s\n", run.ID, run.FinishedAt.Local().Format("2006-01-02 15:04:05 MST"), run.FinishedAt.Sub(run.StartedAt))
	if run.Evaluation.Complete && run.Evaluation.Score != nil {
		fmt.Fprintf(c.OutOrStdout(), "점수: %d/%d\n", *run.Evaluation.Score, run.Evaluation.MaxScore)
	} else {
		fmt.Fprintln(c.OutOrStdout(), "채점 미완료: 확정 총점 없음 (ERROR 또는 진행 중)")
	}
	for _, r := range run.Evaluation.Results {
		fmt.Fprintf(c.OutOrStdout(), "요구사항 %s: %s (%d점)\n", r.CriterionID, r.Status, r.Points)
		if r.Status == model.StatusError {
			fmt.Fprintln(c.OutOrStdout(), "  환경 또는 채점 오류로 판정할 수 없습니다. ckad doctor로 확인하세요.")
			continue
		}
		if r.Status == model.StatusFail {
			if r.Evidence.Expected != nil || r.Evidence.Actual != nil {
				fmt.Fprintf(c.OutOrStdout(), "  기대: %v | 관찰: %v\n", r.Evidence.Expected, r.Evidence.Actual)
			}
			if text := feedback[r.CriterionID]; text != "" {
				fmt.Fprintf(c.OutOrStdout(), "  %s\n", text)
			}
			if r.Reason != "" {
				fmt.Fprintf(c.OutOrStdout(), "  관찰 근거: %s\n", r.Reason)
			}
		}
		if !r.Evidence.ObservedAt.IsZero() {
			fmt.Fprintf(c.OutOrStdout(), "  관찰 시각: %s | UID: %s | generation: %d\n", r.Evidence.ObservedAt.Local().Format("2006-01-02 15:04:05 MST"), r.Evidence.UID, r.Evidence.Generation)
		}
	}
}
func printReference(c *cobra.Command, run model.GradingRun) {
	if run.Evaluation.Complete && run.Evaluation.Score != nil && run.Evaluation.MaxScore > 0 {
		fmt.Fprintf(c.OutOrStdout(), "참고 기준: 66%% | 이번 비율: %.1f%% | 만점 기준: 100%%\n", 100*float64(*run.Evaluation.Score)/float64(run.Evaluation.MaxScore))
	}
}
func printSubmissionTimes(c *cobra.Command, subs []model.Submission) {
	for _, sub := range subs {
		fmt.Fprintf(c.OutOrStdout(), "제출: %s | 시각: %s | 풀이: %s | 초과: %s | 제한 내 제출: %t\n", sub.ID, sub.SubmittedAt.Local().Format("2006-01-02 15:04:05 MST"), sub.SolveDuration, sub.Overtime, sub.WithinLimit)
		if sub.ClockAnomaly {
			fmt.Fprintln(c.OutOrStdout(), "제출 시 시계 역행이 감지되었습니다.")
		}
		if len(sub.ViewedSolutions) > 0 {
			fmt.Fprintf(c.OutOrStdout(), "이미 열람한 해설: %v\n", sub.ViewedSolutions)
		}
	}
}
