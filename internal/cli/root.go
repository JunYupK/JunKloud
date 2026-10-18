//go:build linux

// Package cli presents the terminal workflow. Application packages own
// selection, lifecycle, grading and disclosure; this package renders their
// learner-safe views and never reads frozen grading definitions itself.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/JunYupK/JunKloud/internal/author"
	"github.com/JunYupK/JunKloud/internal/environment"
	"github.com/JunYupK/JunKloud/internal/lock"
	"github.com/JunYupK/JunKloud/internal/problem"
	"github.com/JunYupK/JunKloud/internal/session"
	"github.com/JunYupK/JunKloud/internal/store"
	"github.com/spf13/cobra"
)

type Dependencies struct {
	Session     *session.Service
	Environment *environment.Manager
	Author      *author.Validator
	Store       *store.Store
	Now         func() time.Time
}

// ExitError distinguishes an unmet learning criterion from an invalid
// invocation and an operation that could not be completed.
type ExitError struct {
	Code    int
	Message string
}

func (e *ExitError) Error() string        { return e.Message }
func fail(code int, message string) error { return &ExitError{code, message} }

// ExitCode is stable for scripts: 0 success/full score, 1 unmet criteria,
// 2 usage or mode restriction, 3 environment/storage/grading unavailable.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *ExitError
	if errors.As(err, &e) {
		return e.Code
	}
	return 2
}

// Execute writes diagnostics only to stderr. Native parser errors are also
// replaced with public guidance, never an arbitrary dependency error string.
func Execute(ctx context.Context, root *cobra.Command, args []string) int {
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	var e *ExitError
	if !errors.As(err, &e) {
		e = &ExitError{2, "사용법을 확인하세요: ckad --help"}
	}
	if e.Message != "" {
		fmt.Fprintln(root.ErrOrStderr(), e.Message)
	}
	return e.Code
}

func NewRoot(deps Dependencies, in io.Reader, out, errOut io.Writer) *cobra.Command {
	return newRoot(&deps, in, out, errOut)
}
func newRoot(d *Dependencies, in io.Reader, out, errOut io.Writer) *cobra.Command {
	if d.Now == nil {
		d.Now = time.Now
	}
	root := &cobra.Command{Use: "ckad", Short: "터미널 CKAD 학습", SilenceErrors: true, SilenceUsage: true, CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true}}
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetFlagErrorFunc(func(*cobra.Command, error) error { return fail(2, "옵션을 확인하세요: ckad --help") })
	root.PersistentPreRunE = func(c *cobra.Command, args []string) error {
		if c.Context().Err() != nil {
			return fail(3, "작업이 중단되었습니다. ckad status로 기록을 확인하세요.")
		}
		return validateRequest(c, args)
	}
	root.AddCommand(startCommand(d), statusCommand(d), showCommand(d), markCommand(d), gradeCommand(d), reviewCommand(d), doctorCommand(d), listCommand(d), bankCommand(d), resetCommand(d))
	return root
}

func current(c *cobra.Command, d *Dependencies) (string, error) {
	if d.Store == nil || d.Session == nil {
		return "", fail(3, "학습 저장소를 열 수 없습니다. 상태 경로와 권한을 확인하세요.")
	}
	id, err := d.Store.CurrentSessionID(c.Context())
	if err != nil {
		return "", publicError(err)
	}
	return id, nil
}
func publicError(err error) error {
	if err == nil {
		return nil
	}
	var e *ExitError
	if errors.As(err, &e) {
		return err
	}
	switch {
	case errors.Is(err, environment.ErrNoEnvironment):
		return fail(3, "기록된 전용 환경 ID가 없습니다. 최초 환경 생성이 중단됐다면 ckad doctor로 기록된 생성 작업을 재개하세요. 생성 기록과 리소스를 보존했습니다.")
	case errors.Is(err, session.ErrResetPending):
		return fail(3, "초기화가 중단되었습니다. 기존 풀이와 정리 기록을 보존했습니다. ckad status 확인 후 ckad reset --recover로 재개하세요.")
	case errors.Is(err, environment.ErrCleanupTimeout), errors.Is(err, environment.ErrStorageRetained), errors.Is(err, environment.ErrNotOwned):
		return fail(3, "초기화를 완료하지 못했습니다: "+err.Error()+". 남은 대상과 소유권을 확인한 뒤 ckad reset --recover로 재개하세요.")
	case errors.Is(err, session.ErrPackageMissing), errors.Is(err, session.ErrPackageCorrupt):
		return fail(3, "기록된 문제 패키지가 없거나 손상되어 정확히 재현할 수 없습니다. 원래 패키지를 복구하세요. 다른 버전으로 대체하지 않았습니다.")
	case errors.Is(err, session.ErrResetConfirmation):
		return fail(2, "전체 초기화에는 대상 환경 ID 확인이 필요합니다.")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return fail(3, "작업이 중단되었습니다. ckad status로 기록을 확인하세요.")
	case errors.Is(err, store.ErrNotFound):
		return fail(2, "세션 또는 문제가 없습니다. ckad start 또는 ckad status를 확인하세요.")
	case errors.Is(err, session.ErrDisclosureLocked):
		return fail(2, "모의시험의 해설은 전체 제출을 오류 없이 채점한 후 열 수 있습니다. ckad grade를 실행하세요.")
	case errors.Is(err, session.ErrProblemGradingUnavailable):
		return fail(2, "모의시험은 문제별 채점을 지원하지 않습니다. ckad grade로 전체 제출하세요.")
	case errors.Is(err, session.ErrAnswerChanged), errors.Is(err, session.ErrRetryNotProvable):
		return fail(2, "답안이 바뀌었거나 동일함을 확인할 수 없습니다. ckad grade --resubmit으로 새 제출을 기록하세요.")
	case errors.Is(err, session.ErrGradeInterrupted):
		return fail(3, "이전 채점이 중단되었습니다. ckad grade --retry 또는 수정 후 --resubmit으로 복구하세요.")
	case errors.Is(err, session.ErrRetryUnavailable):
		return fail(3, "오답 재도전은 아직 사용할 수 없습니다. 현재 결과는 ckad review로 확인하세요.")
	case errors.Is(err, problem.ErrInsufficientProblems):
		return fail(2, "출제 가능한 문제가 요청한 수보다 적습니다. ckad list로 검증 상태와 필터를 확인하세요.")
	case errors.Is(err, session.ErrInvalidOptions), errors.Is(err, session.ErrConflictingOptions), errors.Is(err, session.ErrUnknownProblem), errors.Is(err, session.ErrNotStarted), errors.Is(err, session.ErrNotRunning), errors.Is(err, session.ErrNothingToRetry), errors.Is(err, session.ErrNothingToResubmit), errors.Is(err, problem.ErrInvalidCount):
		return fail(2, "현재 모드·상태에서 사용할 수 없는 요청입니다. ckad status와 명령의 --help를 확인하세요.")
	case errors.Is(err, session.ErrSessionActive), errors.Is(err, author.ErrSessionActive):
		return fail(2, "진행 중인 학습이 있습니다. ckad status로 확인하고 학습을 마친 뒤 실행하세요.")
	case errors.Is(err, lock.ErrLocked):
		return fail(3, "다른 준비·채점·검증 작업이 실행 중입니다. 해당 작업이 끝난 뒤 다시 시도하세요. ckad status는 조회할 수 있습니다.")
	case errors.Is(err, session.ErrNotCertified):
		return fail(3, "문제 검증 기록이 없거나 현재 환경과 다릅니다. ckad bank validate <problem-id>로 검증하세요.")
	case errors.Is(err, session.ErrCapabilityUnavailable):
		return fail(3, "필요한 환경 capability가 준비되지 않았습니다. ckad doctor로 확인하세요.")
	case errors.Is(err, session.ErrEnvironmentOccupied):
		return fail(3, "이전 학습 리소스가 남아 있습니다. ckad status 확인 후 ckad reset으로 정리하세요.")
	default:
		return fail(3, "환경·저장소 또는 채점 작업을 완료하지 못했습니다. 상태 경로·문제 은행 경로와 권한을 확인하고 ckad doctor / ckad status로 진단하세요.")
	}
}
