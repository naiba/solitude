package router

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/gofiber/fiber/v2"
	"github.com/naiba/solitudes/pkg/pagination"
	"gorm.io/gorm"

	"github.com/naiba/solitudes"
	"github.com/naiba/solitudes/internal/model"
	"github.com/naiba/solitudes/pkg/translator"
)

func article(c *fiber.Ctx) error {
	var a model.Article
	if err := solitudes.System.DB.Take(&a, "slug = ?", c.Params("slug")).Error; err == gorm.ErrRecordNotFound {
		tr := c.Locals(solitudes.CtxTranslator).(*translator.Translator)
		c.Status(http.StatusNotFound).Render("default/error", injectSiteData(c, fiber.Map{
			"title": tr.T("404_title"),
			"msg":   tr.T("404_msg"),
		}))
		return err
	} else if err != nil {
		return err
	}
	if len(a.Tags) == 0 {
		a.Tags = nil
	}

	var title string
	// load history
	if c.Params("version") != "" {
		version, err := strconv.ParseUint(c.Params("version")[1:], 10, 64)
		if err != nil {
			return err
		}
		if uint(version) == a.Version {
			c.Redirect("/"+a.Slug, http.StatusFound)
			return err
		}
		var history model.ArticleHistory
		if err := solitudes.System.DB.Take(&history, "article_id = ? and version = ?", a.ID, version).Error; err == gorm.ErrRecordNotFound {
			tr := c.Locals(solitudes.CtxTranslator).(*translator.Translator)
			c.Status(http.StatusNotFound).Render("default/error", injectSiteData(c, fiber.Map{
				"title": tr.T("404_title"),
				"msg":   tr.T("404_msg"),
			}))
			return err
		} else if err != nil {
			return err
		}
		a.NewVersion = a.Version
		a.Version = history.Version
		a.Content = history.Content
		a.CreatedAt = history.CreatedAt
		title = fmt.Sprintf("%s v%d", a.Title, a.Version)
	} else {
		title = a.Title
	}
	var wg sync.WaitGroup
	wg.Add(5)
	checkPoolSubmit(&wg, solitudes.System.Pool.Submit(func() {
		relatedChapters(&a)
		wg.Done()
	}))
	checkPoolSubmit(&wg, solitudes.System.Pool.Submit(func() {
		relatedBook(&a)
		wg.Done()
	}))
	checkPoolSubmit(&wg, solitudes.System.Pool.Submit(func() {
		// load prevPost,nextPost
		relatedSiblingArticle(&a)
		wg.Done()
	}))
	checkPoolSubmit(&wg, solitudes.System.Pool.Submit(func() {
		a.GenTOC()
		wg.Done()
	}))
	var pg *pagination.Paginator
	checkPoolSubmit(&wg, solitudes.System.Pool.Submit(func() {
		// load root comments
		pageSlice := c.Query("comment_page")
		var page int64
		if pageSlice != "" {
			page, _ = strconv.ParseInt(pageSlice, 10, 32)
		}
		pg = pagination.Paging(&pagination.Param{
			DB:      solitudes.System.DB.Where("reply_to is null and article_id = ?", a.ID),
			Page:    int(page),
			Limit:   20,
			OrderBy: []string{"created_at DESC"},
		}, &a.Comments)
		// load childComments
		relatedChildComments(&a, a.Comments, true)
		wg.Done()
	}))
	wg.Wait()
	a.RelatedCount(solitudes.System.DB, solitudes.System.Pool, checkPoolSubmit)

	// 检查私有博文
	if a.IsPrivate && !c.Locals(solitudes.CtxAuthorized).(bool) {
		a.Content = "Private Article"
	}

	c.Status(http.StatusOK).Render("default/"+solitudes.TemplateIndex[a.TemplateID], injectSiteData(c, fiber.Map{
		"title":        title,
		"keywords":     a.RawTags,
		"article":      a,
		"comment_page": pg,
	}))
	return nil
}

func relatedSiblingArticle(p *model.Article) (prev model.Article, next model.Article) {
	sibiling, _, _ := solitudes.System.SafeCache.Do(solitudes.CacheKeyPrefixRelatedSiblingArticle+p.ID, func() (interface{}, error) {
		var sb model.SibilingArticle
		if p.BookRefer == nil {
			solitudes.System.DB.Select("id,title,slug").Order("created_at ASC").Take(&sb.Next, "book_refer is null and created_at > ?", p.CreatedAt)
			solitudes.System.DB.Select("id,title,slug").Order("created_at DESC").Where("book_refer is null and created_at < ?", p.CreatedAt).Take(&sb.Prev)
		} else {
			// if this is a book chapter
			solitudes.System.DB.Select("id,title,slug").Order("created_at ASC").Take(&sb.Next, "book_refer = ? and  created_at > ?", p.BookRefer, p.CreatedAt)
			solitudes.System.DB.Select("id,title,slug").Order("created_at DESC").Where("book_refer = ? and  created_at < ?", p.BookRefer, p.CreatedAt).Take(&sb.Prev)
		}
		return sb, nil
	})
	if sibiling != nil {
		x := sibiling.(model.SibilingArticle)
		p.SibilingArticle = &x
	}
	return
}

func relatedChapters(p *model.Article) {
	if p.IsBook {
		chapters, _, _ := solitudes.System.SafeCache.Do(solitudes.CacheKeyPrefixRelatedChapters+p.ID, func() (interface{}, error) {
			return innerRelatedChapters(p.ID), nil
		})
		if chapters != nil {
			x := chapters.([]*model.Article)
			p.Chapters = x
		}
	}
}

func innerRelatedChapters(pid string) (ps []*model.Article) {
	solitudes.System.DB.Order("created_at ASC").Find(&ps, "book_refer=?", pid)
	for i := 0; i < len(ps); i++ {
		if ps[i].IsBook {
			ps[i].Chapters = innerRelatedChapters(ps[i].ID)
		}
	}
	return
}

func relatedBook(p *model.Article) {
	if p.BookRefer != nil {
		book, err, _ := solitudes.System.SafeCache.Do(solitudes.CacheKeyPrefixRelatedArticle+*p.BookRefer, func() (interface{}, error) {
			var book model.Article
			var err error
			if err = solitudes.System.DB.Take(&book, "id = ?", p.BookRefer).Error; err != nil {
				return nil, err
			}
			return book, err
		})
		if err == nil {
			x := book.(model.Article)
			p.Book = &x
		}
	}
}

func relatedChildComments(a *model.Article, cm []*model.Comment, root bool) {
	if root {
		var idMaptoComment = make(map[string]*model.Comment)
		var idArray []string
		// map to index
		for i := 0; i < len(cm); i++ {
			idMaptoComment[cm[i].ID] = cm[i]
			idArray = append(idArray, cm[i].ID)
		}
		var cms []*model.Comment
		solitudes.System.DB.Raw(`WITH RECURSIVE cs AS (SELECT comments.* FROM comments WHERE comments.reply_to in (?) union ALL
		SELECT comments.* FROM comments, cs WHERE comments.reply_to = cs.id)
		SELECT * FROM cs ORDER BY created_at;`, idArray).Scan(&cms)
		// map to index
		for i := 0; i < len(cms); i++ {
			if cms[i].ReplyTo != nil {
				idMaptoComment[cms[i].ID] = cms[i]
			}
		}
		// set child comments
		for i := 0; i < len(cms); i++ {
			if _, has := idMaptoComment[*cms[i].ReplyTo]; has {
				idMaptoComment[*cms[i].ReplyTo].ChildComments =
					append(idMaptoComment[*cms[i].ReplyTo].ChildComments, cms[i])
			}
		}
	}
	for i := 0; i < len(cm); i++ {
		cm[i].Article = a
		if len(cm[i].ChildComments) > 0 {
			relatedChildComments(a, cm[i].ChildComments, false)
			continue
		}
	}
}
