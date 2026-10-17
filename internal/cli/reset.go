//go:build linux

package cli

import (
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/spf13/cobra"
)

func resetCommand(d *Dependencies) *cobra.Command {
	c := &cobra.Command{Use: "reset", Short: "초기화 (Task 13 연결 전에는 사용할 수 없음)", Args: cobra.NoArgs}
	c.Flags().String("problem", "", "훈련 문제 Q-number 또는 problem-id")
	c.Flags().Bool("cluster", false, "전용 클러스터 복구 재생성")
	c.RunE = func(c *cobra.Command, _ []string) error {
		pid, _ := c.Flags().GetString("problem")
		if pid != "" {
			id, err := current(c, d)
			if err != nil {
				return err
			}
			if _, err := resolve(c, d, id, pid); err != nil {
				return err
			}
			st, err := d.Session.Status(c.Context(), id, d.Now())
			if err != nil {
				return publicError(err)
			}
			if st.Session.Mode == model.ModeMock {
				return fail(2, "모의시험은 개별 문제 초기화를 지원하지 않습니다.")
			}
		}
		return fail(3, "reset은 아직 사용할 수 없습니다. Task 13 초기화·보존·복구 연결이 필요합니다. 리소스는 삭제하지 않았습니다.")
	}
	return c
}
