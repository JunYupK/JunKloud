package grader

import (
	"context"
	"errors"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
)

var errTODO = errors.New("not implemented")

type jobArgs struct{}
type cronTemplateArgs struct{}
type cronScheduleArgs struct{}

func parseJobArgs(model.Criterion) (jobArgs, error) { return jobArgs{}, nil }
func parseCronTemplateArgs(model.Criterion) (cronTemplateArgs, error) {
	return cronTemplateArgs{}, nil
}
func parseCronScheduleArgs(model.Criterion, time.Duration) (cronScheduleArgs, error) {
	return cronScheduleArgs{}, nil
}

func (c *ArtifactChecker) checkJob(context.Context, Target, model.Criterion) (model.CheckResult, error) {
	return model.CheckResult{}, errTODO
}
func (c *ArtifactChecker) checkCronTemplate(context.Context, Target, model.Criterion) (model.CheckResult, error) {
	return model.CheckResult{}, errTODO
}
func (c *ArtifactChecker) checkCronSchedule(context.Context, Target, model.Criterion) (model.CheckResult, error) {
	return model.CheckResult{}, errTODO
}
