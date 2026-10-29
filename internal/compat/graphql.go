package compat

import "context"

func (c *Checker) checkGraphQL(ctx context.Context, m *materializer, spec string, res *Result) {
	for _, side := range sides {
		if err := m.write(ctx, side, spec); err != nil {
			c.failed(res, SourceGraphQL, spec, "не удалось подготовить файлы: "+err.Error(), nil)
			return
		}
	}
	argv := []string{"graphql-inspector", "diff", "old/" + spec, "new/" + spec}
	out, ok := c.run(ctx, m.dir, argv, SourceGraphQL, spec, res)
	if !ok {
		return
	}
	breaking, _ := ParseGraphQLInspector(out.Stdout+"\n"+out.Stderr, spec)
	switch {
	case out.ExitCode == 0:
	case out.ExitCode == 1 && len(breaking) > 0:
		res.Changes = append(res.Changes, breaking...)
	default:
		c.failed(res, SourceGraphQL, spec, "graphql-inspector не смог сравнить схемы", out)
	}
}
