//go:build linux

package cli

import (
	"fmt"
	"github.com/spf13/cobra"
)

func listCommand(d *Dependencies) *cobra.Command {
	return &cobra.Command{Use: "list", Short: "현재 출제 가능한 문제·모의시험 조회", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if d.Session == nil {
			return fail(3, "문제 은행과 환경을 열 수 없습니다.")
		}
		catalog, err := d.Session.Catalog(c.Context())
		if err != nil {
			return publicError(err)
		}
		for _, p := range catalog.Problems {
			fmt.Fprintf(c.OutOrStdout(), "%s | %s | %s | %d점\n", p.ID, p.Title, p.Domain, p.Points)
		}
		for _, exam := range catalog.Exams {
			fmt.Fprintf(c.OutOrStdout(), "모의시험 %s | %d문제 | %s\n", exam.ID, len(exam.Problems), exam.Duration)
		}
		for _, x := range catalog.Excluded {
			fmt.Fprintf(c.OutOrStdout(), "출제 불가: %s — %s\n", x.ProblemID, x.Reason)
		}
		return nil
	}}
}
func bankCommand(d *Dependencies) *cobra.Command {
	bank := &cobra.Command{Use: "bank", Short: "문제 은행 작성용 도구"}
	validate := &cobra.Command{Use: "validate <problem-id>", Short: "초기·정답·대표 오답·reset과 parameter 범위 검증", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		if d.Session == nil || d.Author == nil {
			return fail(3, "작성용 검증 환경을 열 수 없습니다.")
		}
		fmt.Fprintln(c.ErrOrStderr(), "문제 검증 중입니다. 진행 중인 학습과 동시에 실행할 수 없습니다.")
		record, err := d.Session.ValidateBank(c.Context(), d.Author, args[0])
		if err != nil {
			return publicError(err)
		}
		if !record.Passed {
			return fail(3, "문제 검증을 통과하지 못했습니다. 작성용 검증 자료를 확인하세요.")
		}
		fmt.Fprintf(c.OutOrStdout(), "검증 완료: %s | %d개 seed | hash %s\n", args[0], len(record.Seeds), record.ProblemHash)
		return nil
	}}
	bank.AddCommand(validate)
	return bank
}
