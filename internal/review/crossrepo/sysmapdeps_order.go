package crossrepo

// System map, phase B: what a counterpart merge request does with a
// contract and the merge order it implies, in Russian (the summary) and
// English (the system prompt).

import (
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
)

// mergeOrder is the merge order a link suggests.
type mergeOrder int

const (
	orderNone mergeOrder = iota
	orderThisFirst
	orderOtherFirst
	orderExpand   // expand/contract: the provider's compatible change first
	orderConflict // the consumer starts using what the provider removes
)

// relation describes what a counterpart merge request does with a
// contract (Russian, name already rendered) and the merge order it
// implies.
func relation(role sysmap.DepRole, act otherAct, name string) (string, mergeOrder) {
	switch role {
	case sysmap.RoleProvidesNew:
		switch act {
		case actAdds:
			return "начинает использовать " + name + ", который добавляет этот MR", orderThisFirst
		case actChanges:
			return "меняет использование " + name + ", который добавляет этот MR", orderThisFirst
		default:
			return "убирает использование " + name + ", который добавляет этот MR", orderNone
		}
	case roleProvidesChanged:
		switch act {
		case actAdds:
			return "начинает использовать " + name + ", который меняет этот MR", orderExpand
		case actChanges:
			return "меняет использование " + name + ", который меняет этот MR", orderExpand
		default:
			return "перестаёт использовать " + name + ", который меняет этот MR", orderOtherFirst
		}
	case sysmap.RoleConsumesNew:
		switch act {
		case actAdds:
			return "добавляет " + name + ", который начинает использовать этот MR", orderOtherFirst
		case actChanges:
			return "меняет " + name + ", который начинает использовать этот MR", orderExpand
		default:
			return "убирает " + name + ", который начинает использовать этот MR", orderConflict
		}
	case sysmap.RoleConsumesDropped:
		switch act {
		case actRemoves:
			return "убирает " + name + ", который перестаёт использовать этот MR", orderThisFirst
		case actChanges:
			return "меняет " + name + ", который перестаёт использовать этот MR", orderNone
		default:
			return "добавляет " + name + ", который перестаёт использовать этот MR", orderNone
		}
	}
	return "", orderNone
}

// relationEN is relation for the system prompt.
func relationEN(role sysmap.DepRole, act otherAct, name string) string {
	verb := map[otherAct]string{actAdds: "adds", actRemoves: "removes", actChanges: "changes"}[act]
	switch role {
	case sysmap.RoleProvidesNew:
		return verb + " a use of " + name + ", which this merge request starts to provide"
	case roleProvidesChanged:
		return verb + " a use of " + name + ", which this merge request changes"
	case sysmap.RoleConsumesNew:
		return verb + " " + name + ", which this merge request starts to use"
	case sysmap.RoleConsumesDropped:
		return verb + " " + name + ", which this merge request stops using"
	}
	return ""
}

// combineOrder merges the orders of one merge request's links.
func combineOrder(links []depLink) mergeOrder {
	set := map[mergeOrder]bool{}
	for _, l := range links {
		if _, o := relation(l.check.contract.Role, l.act, ""); o != orderNone {
			set[o] = true
		}
	}
	switch {
	case set[orderConflict]:
		return orderConflict
	case len(set) == 1:
		for o := range set {
			return o
		}
	case len(set) > 1:
		return orderExpand
	}
	return orderNone
}

// orderText renders the merge order (Russian).
func orderText(o mergeOrder, ref string) string {
	switch o {
	case orderThisFirst:
		return "Порядок: сначала этот MR, потом " + ref + "."
	case orderOtherFirst:
		return "Порядок: сначала " + ref + ", потом этот MR."
	case orderExpand:
		return "Порядок не очевиден: по схеме expand/contract сначала совместимое изменение провайдера (старый и новый вариант вместе), потом потребитель, потом удаление старого."
	case orderConflict:
		return "⚠️ Конфликт: этот MR начинает использовать то, что " + ref + " убирает, — согласуйте изменения."
	}
	return ""
}

func orderTextEN(o mergeOrder, ref string) string {
	switch o {
	case orderThisFirst:
		return " Merge order: this merge request first, then " + ref + "."
	case orderOtherFirst:
		return " Merge order: " + ref + " first, then this merge request."
	case orderExpand:
		return " Merge order unclear: expand/contract (the provider's compatible change first, then the consumer, then the removal)."
	case orderConflict:
		return " Conflict: this merge request starts to use what " + ref + " removes."
	}
	return ""
}
