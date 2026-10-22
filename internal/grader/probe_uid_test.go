package grader

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestProbeUIDReplacementPreserved(t *testing.T) {
	f := newFakePods()
	f.onCreate = terminated(okMessage, 0)
	f.onGet = func(_ int, p *corev1.Pod) { p.UID = types.UID("replacement") }
	_, err := testRunner(t, f).Probe(context.Background(), httpRequest())
	if !errors.Is(err, ErrProbeFailed) || f.remaining() != 1 || f.deletes != 0 {
		t.Fatalf("replacement was trusted/deleted: %v remaining=%d deletes=%d", err, f.remaining(), f.deletes)
	}
}
func TestProbeUnknownCreateRetainsPod(t *testing.T) {
	f := newFakePods()
	f.afterCreateErr = errors.New("request outcome unknown")
	_, err := testRunner(t, f).Probe(context.Background(), httpRequest())
	if !errors.Is(err, ErrProbeFailed) || !strings.Contains(err.Error(), "retain") || f.remaining() != 1 || f.deletes != 0 {
		t.Fatalf("ambiguous pod was adopted/deleted: %v remaining=%d deletes=%d", err, f.remaining(), f.deletes)
	}
}

type collisionPods struct{ *fakePods }

func (f collisionPods) Create(_ context.Context, ns string, p *corev1.Pod) (*corev1.Pod, error) {
	p = p.DeepCopy()
	p.UID = "foreign"
	f.pods[ns+"/"+p.Name] = p
	return nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "pods"}, p.Name)
}
func TestProbeAlreadyExistsPreserved(t *testing.T) {
	f := collisionPods{newFakePods()}
	r, err := newPodProbeRunner(f, WithProbeImage(testProbeImage))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Probe(context.Background(), httpRequest())
	if !errors.Is(err, ErrProbeFailed) || f.remaining() != 1 || f.deletes != 0 {
		t.Fatalf("collision deleted: %v remaining=%d deletes=%d", err, f.remaining(), f.deletes)
	}
}
