//go:build linux

package cli

import (
	"fmt"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/spf13/cobra"
)

func reviewCommand(d *Dependencies) *cobra.Command {
	c := &cobra.Command{Use: "review [session-id]", Short: "제출 결과·실패 근거 조회 (해설은 --solution)", Args: cobra.MaximumNArgs(1)}
	c.Flags().String("problem", "", "Q-number 또는 problem-id")
	c.Flags().Bool("solution", false, "해설·모범 풀이 명시 열람 및 이력 저장")
	c.RunE = func(c *cobra.Command, args []string) error {
		id := ""
		if len(args) > 0 {
			id = args[0]
		} else {
			var err error
			id, err = current(c, d)
			if err != nil {
				return err
			}
		}
		if d.Session == nil {
			return fail(3, "학습 저장소를 열 수 없습니다.")
		}
		pid, _ := c.Flags().GetString("problem")
		if pid != "" {
			p, err := resolve(c, d, id, pid)
			if err != nil {
				return err
			}
			pid = p.ID
		}
		solution, _ := c.Flags().GetBool("solution")
		rv, err := d.Session.Review(c.Context(), id, pid, solution)
		if err != nil {
			return publicError(err)
		}
		st, err := d.Session.Status(c.Context(), id, d.Now())
		if err != nil {
			return publicError(err)
		}
		fmt.Fprintf(c.OutOrStdout(), "복습: %s | 상태: %s | 해설 열람 이력: %t\n", id, rv.State, rv.SolutionViewed)
		printPending(c, rv.Pending)
		if rv.Confirmed != nil {
			fmt.Fprintln(c.OutOrStdout(), "확정 결과:")
			printRun(c, *rv.Confirmed, rv.Feedback)
			if st.Session.Mode == model.ModeMock && pid == "" {
				printReference(c, *rv.Confirmed)
			}
		} else {
			fmt.Fprintln(c.OutOrStdout(), "현재 확정 총점 없음. 개별 훈련 결과를 합산한 총점은 제공하지 않습니다.")
		}
		printSubmissionTimes(c, rv.Submissions)
		for _, run := range rv.Runs {
			if rv.Confirmed == nil || rv.Confirmed.ID != run.ID {
				printRun(c, run, rv.Feedback)
			}
		}
		if len(rv.ReviewRuns) > 0 {
			fmt.Fprintln(c.OutOrStdout(), "모의시험 후 복습 제출:")
			printSubmissionTimes(c, rv.ReviewSubmissions)
			for _, run := range rv.ReviewRuns {
				printRun(c, run, rv.Feedback)
			}
		}
		if solution {
			fmt.Fprintf(c.OutOrStdout(), "\n원인 설명과 진단:\n%s\n모범 풀이:\n%s\n", rv.Explanation, rv.ReferenceSolution)
		}
		return nil
	}
	return c
}
