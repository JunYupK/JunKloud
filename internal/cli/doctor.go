//go:build linux

package cli

import (
	"fmt"
	"github.com/spf13/cobra"
	"sort"
)

func doctorCommand(d *Dependencies) *cobra.Command {
	return &cobra.Command{Use: "doctor", Short: "전용 환경·도구 version·플랫폼 capability 점검", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if d.Session == nil {
			return fail(3, "환경을 열 수 없습니다. Linux 작업 환경과 Docker 연결을 확인하세요.")
		}
		report, err := d.Session.Doctor(c.Context())
		if err != nil {
			return publicError(err)
		}
		fmt.Fprintf(c.OutOrStdout(), "전용 환경: %s\n", report.EnvironmentID)
		keys := make([]string, 0, len(report.Versions))
		for k := range report.Versions {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(c.OutOrStdout(), "%s: %s\n", k, report.Versions[k])
		}
		failed := false
		for _, check := range report.Checks {
			fmt.Fprintf(c.OutOrStdout(), "%s: %s\n", check.Name, check.Status)
			if check.Status != "PASS" {
				failed = true
				fmt.Fprintf(c.ErrOrStderr(), "%s capability를 사용할 수 없습니다. Docker 자원·플랫폼 상태를 확인하고 다시 실행하세요.\n", check.Name)
			}
		}
		if failed {
			return fail(3, "필요한 플랫폼 capability가 준비되지 않았습니다.")
		}
		return nil
	}}
}
