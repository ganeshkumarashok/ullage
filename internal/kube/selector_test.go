package kube

import "testing"

// A PodDisruptionBudget is the only thing standing between "this node looks
// reclaimable" and a drain that hangs forever, so its selector has to be
// evaluated properly rather than approximated.
//
// Two of these cases were wrong before this test existed. An expression-only
// selector -- entirely ordinary, and what every `app in (a, b)` budget looks
// like -- reported matched=false, so the budget was silently ignored. And an
// empty selector, which Kubernetes documents as selecting *every* pod in the
// namespace, also reported false, which is the exact opposite of its meaning.
func TestLabelSelectorMatches(t *testing.T) {
	labels := map[string]string{"app": "trainer", "tier": "gpu"}

	cases := []struct {
		name    string
		sel     *LabelSelector
		matched bool
		certain bool
		why     string
	}{{
		name: "a null selector matches no pods",
		sel:  nil, matched: false, certain: true,
		why: "policy/v1 documents a null selector as matching nothing",
	}, {
		name: "an empty selector matches every pod in the namespace",
		sel:  &LabelSelector{}, matched: true, certain: true,
		why: "policy/v1 documents `{}` as selecting all pods; it is the blanket budget",
	}, {
		name:    "matchLabels, all satisfied",
		sel:     &LabelSelector{MatchLabels: map[string]string{"app": "trainer"}},
		matched: true, certain: true,
	}, {
		name:    "matchLabels, one wrong",
		sel:     &LabelSelector{MatchLabels: map[string]string{"app": "serving"}},
		matched: false, certain: true,
	}, {
		name: "In, value present",
		sel: &LabelSelector{MatchExpressions: []LabelSelectorRequirement{
			{Key: "app", Operator: "In", Values: []string{"trainer", "eval"}}}},
		matched: true, certain: true,
		why: "an expression-only selector is ordinary and used to be ignored entirely",
	}, {
		name: "In, value absent from the set",
		sel: &LabelSelector{MatchExpressions: []LabelSelectorRequirement{
			{Key: "app", Operator: "In", Values: []string{"serving"}}}},
		matched: false, certain: true,
	}, {
		name: "In, key not on the pod",
		sel: &LabelSelector{MatchExpressions: []LabelSelectorRequirement{
			{Key: "absent", Operator: "In", Values: []string{"trainer"}}}},
		matched: false, certain: true,
	}, {
		name: "NotIn, value present in the set",
		sel: &LabelSelector{MatchExpressions: []LabelSelectorRequirement{
			{Key: "app", Operator: "NotIn", Values: []string{"trainer"}}}},
		matched: false, certain: true,
	}, {
		name: "NotIn, key absent",
		sel: &LabelSelector{MatchExpressions: []LabelSelectorRequirement{
			{Key: "absent", Operator: "NotIn", Values: []string{"trainer"}}}},
		matched: true, certain: true,
		why: "a pod without the key is not in the set, so NotIn is satisfied",
	}, {
		name: "Exists",
		sel: &LabelSelector{MatchExpressions: []LabelSelectorRequirement{
			{Key: "tier", Operator: "Exists"}}},
		matched: true, certain: true,
	}, {
		name: "Exists, absent",
		sel: &LabelSelector{MatchExpressions: []LabelSelectorRequirement{
			{Key: "absent", Operator: "Exists"}}},
		matched: false, certain: true,
	}, {
		name: "DoesNotExist",
		sel: &LabelSelector{MatchExpressions: []LabelSelectorRequirement{
			{Key: "absent", Operator: "DoesNotExist"}}},
		matched: true, certain: true,
	}, {
		name: "DoesNotExist, present",
		sel: &LabelSelector{MatchExpressions: []LabelSelectorRequirement{
			{Key: "tier", Operator: "DoesNotExist"}}},
		matched: false, certain: true,
	}, {
		name: "matchLabels and matchExpressions are ANDed",
		sel: &LabelSelector{
			MatchLabels: map[string]string{"app": "trainer"},
			MatchExpressions: []LabelSelectorRequirement{
				{Key: "tier", Operator: "In", Values: []string{"cpu"}}}},
		matched: false, certain: true,
	}, {
		name: "an operator we do not recognise is not an answer",
		sel: &LabelSelector{MatchExpressions: []LabelSelectorRequirement{
			{Key: "app", Operator: "Sometimes", Values: []string{"trainer"}}}},
		matched: true, certain: false,
		why: "saying 'does not match' would discard a real budget",
	}, {
		name: "operators are matched case-insensitively",
		sel: &LabelSelector{MatchExpressions: []LabelSelectorRequirement{
			{Key: "tier", Operator: "exists"}}},
		matched: true, certain: true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matched, certain := tc.sel.Matches(labels)
			if matched != tc.matched || certain != tc.certain {
				msg := ""
				if tc.why != "" {
					msg = " — " + tc.why
				}
				t.Fatalf("Matches(%v) = (matched=%v, certain=%v), want (%v, %v)%s",
					labels, matched, certain, tc.matched, tc.certain, msg)
			}
		})
	}
}
