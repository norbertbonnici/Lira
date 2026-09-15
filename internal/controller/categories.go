package controller

import (
	"net/http"
	"strings"

	"lira/internal/model"
)

func catKind(s string) string {
	if s == "income" {
		return "income"
	}
	return "expense"
}

func (c *Controller) Categories(w http.ResponseWriter, r *http.Request) {
	cats, _ := c.store.ListCategories()
	usage, _ := c.store.CategoryUsage()
	type row struct {
		model.Category
		Count int
	}
	rows := make([]row, 0, len(cats))
	for _, ct := range cats {
		rows = append(rows, row{ct, usage[ct.ID]})
	}
	c.render(w, r, "categories.html", map[string]any{"Rows": rows})
}

func (c *Controller) CategoryCreate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if name := strings.TrimSpace(r.FormValue("name")); name != "" {
		_ = c.store.CreateCategory(name, catKind(r.FormValue("kind")))
	}
	http.Redirect(w, r, "/categories", http.StatusSeeOther)
}

func (c *Controller) CategoryEdit(w http.ResponseWriter, r *http.Request) {
	ct, err := c.store.GetCategory(pathID(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	c.render(w, r, "category_form.html", map[string]any{"Category": ct})
}

func (c *Controller) CategoryUpdate(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	id := pathID(r)
	if name := strings.TrimSpace(r.FormValue("name")); name != "" {
		_ = c.store.UpdateCategory(id, name, catKind(r.FormValue("kind")))
	}
	http.Redirect(w, r, "/categories", http.StatusSeeOther)
}

func (c *Controller) CategoryDelete(w http.ResponseWriter, r *http.Request) {
	if !c.checkCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	_ = c.store.DeleteCategory(pathID(r))
	http.Redirect(w, r, "/categories", http.StatusSeeOther)
}
