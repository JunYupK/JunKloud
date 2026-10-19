//go:build linux

package cli

import (
	"fmt"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/spf13/cobra"
)

func imageCommand(d *Dependencies) *cobra.Command {
	parent := &cobra.Command{Use: "image", Short: "학습 이미지 빌드"}
	c := &cobra.Command{Use: "build", Short: "현재 풀이의 Dockerfile로 소유권을 기록한 이미지 빌드", Args: cobra.NoArgs}
	c.Flags().String("problem", "", "Q-number 또는 problem-id (필수)")
	c.Flags().String("context", ".", "현재 attempt 아래의 빌드 context")
	c.Flags().String("tag", "", "이미지 reference (필수)")
	c.RunE = func(c *cobra.Command, _ []string) error {
		id, err := current(c, d)
		if err != nil {
			return err
		}
		pid, _ := c.Flags().GetString("problem")
		tag, _ := c.Flags().GetString("tag")
		dir, _ := c.Flags().GetString("context")
		if pid == "" || tag == "" {
			return fail(2, "--problem과 --tag를 지정하세요.")
		}
		p, err := resolve(c, d, id, pid)
		if err != nil {
			return err
		}
		image, err := d.Session.BuildImage(c.Context(), id, session.ImageBuildOptions{ProblemID: p.ID, ContextDir: dir, Reference: tag})
		if err != nil {
			return publicError(err)
		}
		fmt.Fprintf(c.OutOrStdout(), "이미지 빌드 완료: %s (%s)\n", image.Reference, image.ID)
		return nil
	}
	parent.AddCommand(c)
	return parent
}
