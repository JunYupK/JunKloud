package grader

import (
	"context"
	"errors"
	"time"

	"github.com/JunYupK/JunKloud/internal/model"
)

var errTODO = errors.New("not implemented")

type logsArgs struct{}
type imageArgs struct{}
type helmArgs struct{}
type kustomizeArgs struct{}
type jobArgs struct{}
type cronTemplateArgs struct{}
type cronScheduleArgs struct{}

// ImageRunner runs a learner image in the cluster.
type ImageRunner interface{}

func parseLogsArgs(model.Criterion) (logsArgs, error)           { return logsArgs{}, nil }
func parseImageArgs(model.Criterion) (imageArgs, error)         { return imageArgs{}, nil }
func parseHelmArgs(model.Criterion) (helmArgs, error)           { return helmArgs{}, nil }
func parseKustomizeArgs(model.Criterion) (kustomizeArgs, error) { return kustomizeArgs{}, nil }
func parseJobArgs(model.Criterion) (jobArgs, error)             { return jobArgs{}, nil }
func parseCronTemplateArgs(model.Criterion) (cronTemplateArgs, error) {
	return cronTemplateArgs{}, nil
}
func parseCronScheduleArgs(model.Criterion, time.Duration) (cronScheduleArgs, error) {
	return cronScheduleArgs{}, nil
}

func (c *ArtifactChecker) checkLogs(context.Context, Target, model.Criterion) (model.CheckResult, error) {
	return model.CheckResult{}, errTODO
}
func (c *ArtifactChecker) checkImage(context.Context, Target, model.Criterion) (model.CheckResult, error) {
	return model.CheckResult{}, errTODO
}
func (c *ArtifactChecker) checkHelm(context.Context, Target, model.Criterion) (model.CheckResult, error) {
	return model.CheckResult{}, errTODO
}
func (c *ArtifactChecker) checkKustomize(context.Context, Target, model.Criterion) (model.CheckResult, error) {
	return model.CheckResult{}, errTODO
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
