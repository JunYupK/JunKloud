//go:build linux

package cli

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/spf13/cobra"
)

func statusCommand(d *Dependencies) *cobra.Command {
	return &cobra.Command{Use: "status", Short: "학습 모드·목록·시간·복구 상태 조회", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		id, err := current(c, d)
		if err != nil {
			return err
		}
		st, err := d.Session.Status(c.Context(), id, d.Now())
		if err != nil {
			return publicError(err)
		}
		fmt.Fprintf(c.OutOrStdout(), "세션: %s | 모드: %s | 상태: %s\n경과: %s | 초과: %s | 제한: %s\n", id, st.Session.Mode, st.Session.State, st.Elapsed, st.Overtime, st.Session.DurationLimit)
		if st.ClockAnomaly {
			fmt.Fprintln(c.OutOrStdout(), "시계 역행이 감지되었습니다.")
		}
		for i, p := range st.Session.Problems {
			mark := ""
			if p.Marked {
				mark = " [review]"
			}
			fmt.Fprintf(c.OutOrStdout(), "Q%d %s | %d점 | %s%s\n", i+1, p.ID, p.Points, p.Title, mark)
		}
		printPending(c, st.Pending)
		return nil
	}}
}
func resolve(c *cobra.Command, d *Dependencies, sid, pid string) (session.ProblemView, error) {
	st, err := d.Session.Status(c.Context(), sid, d.Now())
	if err != nil {
		return session.ProblemView{}, publicError(err)
	}
	if strings.HasPrefix(pid, "Q") {
		if n, err := strconv.Atoi(strings.TrimPrefix(pid, "Q")); err == nil && n > 0 && n <= len(st.Session.Problems) {
			return st.Session.Problems[n-1], nil
		}
	}
	for _, p := range st.Session.Problems {
		if p.ID == pid {
			return p, nil
		}
	}
	return session.ProblemView{}, publicError(session.ErrUnknownProblem)
}
func showCommand(d *Dependencies) *cobra.Command {
	return &cobra.Command{Use: "show <Q-number|problem-id>", Short: "영어 문제·대상 context·namespace·제공 파일 조회", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		id, err := current(c, d)
		if err != nil {
			return err
		}
		p, err := resolve(c, d, id, args[0])
		if err != nil {
			return err
		}
		if p.Question == "" {
			return publicError(session.ErrNotStarted)
		}
		fmt.Fprintf(c.OutOrStdout(), "%s — %s\nContext: %s\nNamespace: %s\n\n%s\n", p.ID, p.Title, p.Context, p.Namespace, p.Question)
		names := make([]string, 0, len(p.Assets))
		for name := range p.Assets {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(c.OutOrStdout(), "Provided file: %s\n%s\n", name, p.Assets[name])
		}
		return nil
	}}
}
func markCommand(d *Dependencies) *cobra.Command {
	c := &cobra.Command{Use: "mark <Q-number|problem-id>", Short: "다시 볼 문제 표시·해제 (채점하지 않음)", Args: cobra.ExactArgs(1)}
	c.Flags().Bool("review", false, "다시 볼 문제로 표시")
	c.Flags().Bool("clear", false, "표시 해제")
	c.RunE = func(c *cobra.Command, args []string) error {
		id, err := current(c, d)
		if err != nil {
			return err
		}
		p, err := resolve(c, d, id, args[0])
		if err != nil {
			return err
		}
		marked, _ := c.Flags().GetBool("review")
		if err := d.Session.Mark(c.Context(), id, p.ID, marked); err != nil {
			return publicError(err)
		}
		fmt.Fprintf(c.OutOrStdout(), "표시 저장: %s (review=%t)\n", p.ID, marked)
		return nil
	}
	return c
}

var recoveryCommand = regexp.MustCompile(`ckad (?:reset --recover|grade(?: --problem [a-z0-9][a-z0-9-]*)?(?: --(?:retry|resubmit))?)`)

func printPending(c *cobra.Command, pending []session.Pending) {
	for _, p := range pending {
		fmt.Fprintf(c.OutOrStdout(), "미완료 작업: %s | %s | %s\n", p.OperationID, p.Phase, p.Since.Local().Format("2006-01-02 15:04:05 MST"))
		// The service has already checked which commands are legal for this
		// interrupted scope. Preserve that decision, including problem scope
		// and unavailable technical retries, without printing internal prose.
		commands := recoveryCommand.FindAllString(p.Guidance, -1)
		if len(commands) == 0 {
			fmt.Fprintln(c.OutOrStdout(), "현재 상태에서 사용할 수 있는 채점 복구 명령이 없습니다.")
			continue
		}
		fmt.Fprintf(c.OutOrStdout(), "허용되는 복구 명령: %s\n", strings.Join(commands, " / "))
	}
}
