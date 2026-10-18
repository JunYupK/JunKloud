//go:build linux

package cli

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/spf13/cobra"
)

func resetCommand(d *Dependencies) *cobra.Command {
	c := &cobra.Command{Use: "reset", Short: "소유 리소스를 정리하고 풀이·기록 보존", Args: cobra.NoArgs}
	c.Flags().String("problem", "", "훈련 문제 Q-number 또는 problem-id")
	c.Flags().Bool("cluster", false, "전용 클러스터 복구 재생성")
	c.Flags().Bool("recover", false, "기록된 미완료 reset 재개")
	c.RunE = func(c *cobra.Command, _ []string) error {
		recover, _ := c.Flags().GetBool("recover")
		cluster, _ := c.Flags().GetBool("cluster")
		if d.Session == nil || d.Store == nil {
			return publicError(errors.New("missing service"))
		}
		sid, err := d.Store.CurrentSessionID(c.Context())
		var st session.Status
		if errors.Is(err, store.ErrNotFound) && (cluster || recover) && d.Environment != nil {
			id, e := d.Environment.RecordedIdentity()
			if e != nil {
				return publicError(e)
			}
			st.Session.EnvironmentID = id.ID
		} else if err != nil {
			return publicError(err)
		} else {
			st, err = d.Session.Status(c.Context(), sid, d.Now())
			if err != nil {
				return publicError(err)
			}
		}
		if recover {
			return publicError(d.Session.Recover(c.Context(), st.Session.EnvironmentID))
		}
		pid, _ := c.Flags().GetString("problem")
		opts := session.ResetOptions{Cluster: cluster}
		if pid != "" {
			p, resolveErr := resolve(c, d, sid, pid)
			opts.ProblemID, err = p.ID, resolveErr
			if err != nil {
				return err
			}
		} else {
			fmt.Fprintf(c.OutOrStdout(), "세션 %s를 포기하고 환경 %s의 소유 리소스를 정리합니다. 풀이·성적·개인 설정은 보존됩니다.\n", sid, st.Session.EnvironmentID)
			if cluster {
				fmt.Fprintln(c.OutOrStdout(), "전용 클러스터 전체를 삭제하고 재생성합니다.")
			}
			fmt.Fprint(c.OutOrStdout(), "확인하려면 환경 ID를 입력하세요: ")
			answers := make(chan string, 1)
			go func() { line, _ := bufio.NewReader(c.InOrStdin()).ReadString('\n'); answers <- strings.TrimSpace(line) }()
			select {
			case <-c.Context().Done():
				return publicError(c.Context().Err())
			case opts.ConfirmedEnvironmentID = <-answers:
			}
			if opts.ConfirmedEnvironmentID != st.Session.EnvironmentID {
				return fail(2, "확인이 일치하지 않아 초기화하지 않았습니다.")
			}
		}
		if err = d.Session.Reset(c.Context(), sid, opts); err != nil {
			return publicError(err)
		}
		fmt.Fprintln(c.OutOrStdout(), "초기화 완료. 이전 풀이와 학습 기록은 보존했습니다.")
		return nil
	}
	return c
}
