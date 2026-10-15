package grader

// object is a decoded Kubernetes object: the JSON document as nested
// map[string]any / []any / string / bool / int64 / float64. The grader's
// public contract uses plain maps so no client-library type leaks out.
type object = map[string]any

func metaOf(obj object) map[string]any {
	m, _ := obj["metadata"].(map[string]any)
	return m
}

func uidOf(obj object) string {
	s, _ := metaOf(obj)["uid"].(string)
	return s
}

func nameOf(obj object) string {
	s, _ := metaOf(obj)["name"].(string)
	return s
}

func generationOf(obj object) int64 {
	if r, ok := ratOf(metaOf(obj)["generation"]); ok && r.IsInt() {
		return r.Num().Int64()
	}
	return 0
}

func stringAt(obj object, path ...string) string {
	v, _ := valueAt(obj, path...).(string)
	return v
}

func mapAt(obj object, path ...string) map[string]any {
	m, _ := valueAt(obj, path...).(map[string]any)
	return m
}

func listAt(obj object, path ...string) []any {
	l, _ := valueAt(obj, path...).([]any)
	return l
}

func valueAt(obj object, path ...string) any {
	var cur any = obj
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}
