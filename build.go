package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brandur/modulir"
	"github.com/brandur/modulir/modules/mace"
	"github.com/brandur/modulir/modules/mfile"
	"github.com/brandur/modulir/modules/mmarkdown"
	"github.com/brandur/modulir/modules/mtoc"
	"github.com/brandur/modulir/modules/myaml"
	"github.com/brandur/sorg/modules/sassets"
	"github.com/brandur/sorg/modules/satom"
	"github.com/brandur/sorg/modules/scommon"
	"github.com/brandur/sorg/modules/smarkdown"
	"github.com/brandur/sorg/modules/spassages"
	"github.com/brandur/sorg/modules/squantified"
	"github.com/brandur/sorg/modules/stalks"
	"github.com/brandur/sorg/modules/stemplate"
	_ "github.com/lib/pq"
	gocache "github.com/patrickmn/go-cache"
	"github.com/pkg/errors"
)

//////////////////////////////////////////////////////////////////////////////
//
//
//
// Constants
//
//
//
//////////////////////////////////////////////////////////////////////////////

// A set of tag constants to hopefully help ensure that this set doesn't grow
// very much.
const (
	tagPostgres Tag = "postgres"
)

//////////////////////////////////////////////////////////////////////////////
//
//
//
// Variables
//
//
//
//////////////////////////////////////////////////////////////////////////////

// These are all objects that are persisted between build loops so that if
// necessary we can rebuild jobs that depend on them like index pages without
// reparsing all the source material. In each case we try to only reparse the
// sources if those source files actually changed.
var (
	articles  []*Article
	fragments []*Fragment
	passages  []*spassages.Passage
	pages     map[string]*Page
	photos    []*Photo
	sequences = make(map[string][]*Photo)
	talks     []*stalks.Talk
)

// A database connection opened to a Black Swan database if one was configured.
var db *sql.DB

// List of partial views. If any of these changes we rebuild pretty much
// everything. Even though some of those changes will false positives, the
// partials are used pervasively enough, and change infrequently enough, that
// it's worth the tradeoff. This variable is a global because so many render
// functions access it.
var partialViews []string

// An expiring cache that tracks the current state of marker files for photos.
// Going to the filesystem on every build loop is relatively slow/expensive, so
// this helps speed up the build loop.
//
// Arguments are (defaultExpiration, cleanupInterval).
var photoMarkerCache = gocache.New(5*time.Minute, 10*time.Minute)

//////////////////////////////////////////////////////////////////////////////
//
//
//
// Build function
//
//
//
//////////////////////////////////////////////////////////////////////////////

func build(c *modulir.Context) []error {
	//
	// PHASE 0: Setup
	//
	// (No jobs should be enqueued here.)
	//

	c.Log.Debugf("Running build loop")

	// This is where we stored "versioned" assets like compiled JS and CSS.
	// These assets have a release number that we can increment and by
	// extension quickly invalidate.
	versionedAssetsDir := path.Join(c.TargetDir, "assets", Release)

	{
		// Only open the first time
		if conf.BlackSwanDatabaseURL != "" {
			if db == nil {
				var err error
				db, err = sql.Open("postgres", conf.BlackSwanDatabaseURL)
				if err != nil {
					return []error{err}
				}
			}
		} else {
			c.Log.Infof("No database set; will not render database-backed views")
		}
	}

	// Generate a list of partial views.
	{
		partialViews = nil

		sources, err := mfile.ReadDirWithOptions(c, c.SourceDir+"/views",
			&mfile.ReadDirOptions{ShowMeta: true})
		if err != nil {
			return []error{err}
		}

		for _, source := range sources {
			if strings.HasPrefix(filepath.Base(source), "_") {
				partialViews = append(partialViews, source)
			}
		}
	}

	//
	// PHASE 1
	//
	// The build is broken into phases because some jobs depend on jobs that
	// ran before them. For example, we need to parse all our article metadata
	// before we can create an article index and render the home page (which
	// contains a short list of articles).
	//
	// After each phase, we call `Wait` on our context which will wait for the
	// worker pool to finish all its current work and restart it to accept new
	// jobs after it has.
	//
	// The general rule is to make sure that work is done as early as it
	// possibly can be. e.g. Jobs with no dependencies should always run in
	// phase 1. Try to make sure that as few phases as necessary
	//

	//
	// Common directories
	//
	// Create these outside of the job system because jobs below may depend on
	// their existence.
	//

	{
		commonDirs := []string{
			c.TargetDir,
			c.TargetDir + "/articles",
			c.TargetDir + "/fragments",
			c.TargetDir + "/passages",
			c.TargetDir + "/photos",
			c.TargetDir + "/reading",
			c.TargetDir + "/runs",
			c.TargetDir + "/twitter",
			scommon.TempDir,
			versionedAssetsDir,
		}
		for _, dir := range commonDirs {
			err := mfile.EnsureDir(c, dir)
			if err != nil {
				return []error{nil}
			}
		}
	}

	//
	// Symlinks
	//

	{
		commonSymlinks := [][2]string{
			{c.SourceDir + "/content/fonts", c.TargetDir + "/assets/fonts"},
			{c.SourceDir + "/content/images", c.TargetDir + "/assets/images"},

			// For backwards compatibility as many emails with this style of path
			// have already gone out.
			{c.SourceDir + "/content/images/passages", c.TargetDir + "/assets/passages"},

			{c.SourceDir + "/content/photographs", c.TargetDir + "/photographs"},
		}
		for _, link := range commonSymlinks {
			err := mfile.EnsureSymlink(c, link[0], link[1])
			if err != nil {
				return []error{nil}
			}
		}
	}

	//
	// Articles
	//

	var articlesChanged bool
	var articlesMu sync.Mutex

	{
		sources, err := mfile.ReadDir(c, c.SourceDir+"/content/articles")
		if err != nil {
			return []error{err}
		}

		if conf.Drafts {
			drafts, err := mfile.ReadDir(c, c.SourceDir+"/content/drafts")
			if err != nil {
				return []error{err}
			}
			sources = append(sources, drafts...)
		}

		for _, s := range sources {
			source := s

			name := fmt.Sprintf("article: %s", filepath.Base(source))
			c.AddJob(name, func() (bool, error) {
				return renderArticle(c, source,
					&articles, &articlesChanged, &articlesMu)
			})
		}
	}

	//
	// Fragments
	//

	var fragmentsChanged bool
	var fragmentsMu sync.Mutex

	{
		sources, err := mfile.ReadDir(c, c.SourceDir+"/content/fragments")
		if err != nil {
			return []error{err}
		}

		if conf.Drafts {
			drafts, err := mfile.ReadDir(c, c.SourceDir+"/content/fragments-drafts")
			if err != nil {
				return []error{err}
			}
			sources = append(sources, drafts...)
		}

		for _, s := range sources {
			source := s

			name := fmt.Sprintf("fragment: %s", filepath.Base(source))
			c.AddJob(name, func() (bool, error) {
				return renderFragment(c, source,
					&fragments, &fragmentsChanged, &fragmentsMu)
			})
		}
	}

	//
	// Javascripts
	//

	{
		c.AddJob("javascripts", func() (bool, error) {
			return compileJavascripts(c, versionedAssetsDir)
		})
	}

	//
	// Pages (read `_meta.yaml`)
	//

	var pagesChanged bool

	{
		c.AddJob("pages _meta.yaml", func() (bool, error) {
			source := c.SourceDir + "/pages/_meta.yaml"

			if !c.Changed(source) && !c.Forced() {
				return false, nil
			}

			err := myaml.ParseFile(
				c, c.SourceDir+"/pages/_meta.yaml", &pages)
			if err != nil {
				return true, err
			}

			pagesChanged = true
			return true, nil
		})
	}

	//
	// Passages
	//

	var passagesChanged bool
	var passagesMu sync.Mutex

	{
		sources, err := mfile.ReadDir(c, c.SourceDir+"/content/passages")
		if err != nil {
			return []error{err}
		}

		if conf.Drafts {
			drafts, err := mfile.ReadDir(c, c.SourceDir+"/content/passages-drafts")
			if err != nil {
				return []error{err}
			}
			sources = append(sources, drafts...)
		}

		for _, s := range sources {
			source := s

			name := fmt.Sprintf("passage: %s", filepath.Base(source))
			c.AddJob(name, func() (bool, error) {
				return renderPassage(c, source,
					&passages, &passagesChanged, &passagesMu)
			})
		}
	}

	//
	// Photos (read `_meta.yaml`)
	//

	var photosChanged bool

	{
		c.AddJob("photos _meta.yaml", func() (bool, error) {
			source := c.SourceDir + "/content/photographs/_meta.yaml"

			if !c.Changed(source) && !c.Forced() {
				return false, nil
			}

			var photosWrapper PhotoWrapper
			err := myaml.ParseFile(c, source, &photosWrapper)
			if err != nil {
				return true, err
			}

			photos = photosWrapper.Photos
			photosChanged = true
			return true, nil
		})
	}

	//
	// Reading
	//

	{
		c.AddJob("reading", func() (bool, error) {
			return c.AllowError(renderReading(c, db)), nil
		})
	}

	//
	// Robots.txt
	//

	{
		c.AddJob("robots.txt", func() (bool, error) {
			return renderRobotsTxt(c)
		})
	}

	//
	// Runs
	//

	{
		c.AddJob("runs", func() (bool, error) {
			return c.AllowError(renderRuns(c, db)), nil
		})
	}

	//
	// Sequences (read `_meta.yaml`)
	//

	sequencesChanged := make(map[string]bool)

	{
		sources, err := mfile.ReadDir(c, c.SourceDir+"/content/sequences")
		if err != nil {
			return []error{err}
		}

		if conf.Drafts {
			drafts, err := mfile.ReadDir(c, c.SourceDir+"/content/sequences-drafts")
			if err != nil {
				return []error{err}
			}
			sources = append(sources, drafts...)
		}

		for _, s := range sources {
			sequencePath := s

			name := fmt.Sprintf("sequence %s _meta.yaml", filepath.Base(sequencePath))
			c.AddJob(name, func() (bool, error) {
				source := sequencePath + "/_meta.yaml"

				if !c.Changed(source) && !c.Forced() {
					return false, nil
				}

				slug := path.Base(sequencePath)

				var photosWrapper PhotoWrapper
				err = myaml.ParseFile(c, source, &photosWrapper)
				if err != nil {
					return true, err
				}

				sequences[slug] = photosWrapper.Photos
				sequencesChanged[slug] = true
				return true, nil
			})
		}
	}

	//
	// Stylesheets
	//

	{
		c.AddJob("stylesheets", func() (bool, error) {
			return compileStylesheets(c, versionedAssetsDir)
		})
	}

	//
	// Talks
	//

	var talksChanged bool
	var talksMu sync.Mutex

	{
		sources, err := mfile.ReadDir(c, c.SourceDir+"/content/talks")
		if err != nil {
			return []error{err}
		}

		if conf.Drafts {
			drafts, err := mfile.ReadDir(c, c.SourceDir+"/content/talks-drafts")
			if err != nil {
				return []error{err}
			}
			sources = append(sources, drafts...)
		}

		for _, s := range sources {
			source := s

			name := fmt.Sprintf("talk: %s", filepath.Base(source))
			c.AddJob(name, func() (bool, error) {
				return renderTalk(c, source, &talks, &talksChanged, &talksMu)
			})
		}
	}

	//
	// Twitter
	//

	{
		c.AddJob("twitter", func() (bool, error) {
			return c.AllowError(renderTwitter(c, db)), nil
		})
	}

	//
	//
	//
	// PHASE 2
	//
	//
	//

	if errors := c.Wait(); errors != nil {
		c.Log.Errorf("Cancelling next phase due to build errors")
		return errors
	}

	// Various sorts for anything that might need it.
	{
		sortArticles(articles)
		sortFragments(fragments)
		sortPassages(passages)
		sortPhotos(photos)
		sortTalks(talks)
	}

	//
	// Articles
	//

	// Index
	{
		c.AddJob("articles index", func() (bool, error) {
			return renderArticlesIndex(c, articles,
				articlesChanged)
		})
	}

	// Feed (all)
	{
		c.AddJob("articles feed", func() (bool, error) {
			return renderArticlesFeed(c, articles, nil,
				articlesChanged)
		})
	}

	// Feed (Postgres)
	{
		c.AddJob("articles feed (postgres)", func() (bool, error) {
			return renderArticlesFeed(c, articles, tagPointer(tagPostgres),
				articlesChanged)
		})
	}

	//
	// Fragments
	//

	// Index
	{
		c.AddJob("fragments index", func() (bool, error) {
			return renderFragmentsIndex(c, fragments,
				fragmentsChanged)
		})
	}

	// Feed
	{
		c.AddJob("fragments feed", func() (bool, error) {
			return renderFragmentsFeed(c, fragments,
				fragmentsChanged)
		})
	}

	//
	// Home
	//

	{
		c.AddJob("home", func() (bool, error) {
			return renderHome(c, articles, fragments, photos,
				articlesChanged, fragmentsChanged, photosChanged)
		})
	}

	//
	// Pages (render each view)
	//

	{
		sources, err := mfile.ReadDir(c, c.SourceDir+"/pages")
		if err != nil {
			return []error{err}
		}

		for _, s := range sources {
			source := s

			name := fmt.Sprintf("page: %s", filepath.Base(source))
			c.AddJob(name, func() (bool, error) {
				return renderPage(c, source, pages,
					pagesChanged)
			})
		}
	}

	//
	// Passages
	//

	{
		c.AddJob("passages index", func() (bool, error) {
			return renderPassagesIndex(c, passages,
				passagesChanged)
		})
	}

	//
	// Photos (index / fetch + resize)
	//

	// Photo index
	{
		c.AddJob("photos index", func() (bool, error) {
			return renderPhotoIndex(c, photos,
				photosChanged)
		})
	}

	// Photo fetch + resize
	{
		for _, p := range photos {
			photo := p

			name := fmt.Sprintf("photo: %s", photo.Slug)
			c.AddJob(name, func() (bool, error) {
				return fetchAndResizePhoto(c, c.SourceDir+"/content/photographs", photo)
			})
		}
	}

	//
	// Sequences (index / fetch + resize)
	//

	{
		for s, p := range sequences {
			slug := s
			photos := p

			var err error
			err = mfile.EnsureDir(c, c.TargetDir+"/sequences/"+slug)
			if err != nil {
				return []error{err}
			}
			err = mfile.EnsureDir(c, c.SourceDir+"/content/photographs/sequences/"+slug)
			if err != nil {
				return []error{err}
			}

			for _, p := range photos {
				photo := p

				// Sequence page
				name := fmt.Sprintf("sequence %s: %s", slug, photo.Slug)
				c.AddJob(name, func() (bool, error) {
					return renderSequence(c, slug, photo,
						sequencesChanged[slug])
				})

				// Sequence fetch + resize
				name = fmt.Sprintf("sequence %s photo: %s", slug, photo.Slug)
				c.AddJob(name, func() (bool, error) {
					return fetchAndResizePhoto(c, c.SourceDir+"/content/photographs/sequences/"+slug, photo)
				})
			}
		}
	}

	return nil
}

//////////////////////////////////////////////////////////////////////////////
//
//
//
// Types
//
//
//
//////////////////////////////////////////////////////////////////////////////

// Article represents an article to be rendered.
type Article struct {
	// Attributions are any attributions for content that may be included in
	// the article (like an image in the header for example).
	Attributions string `yaml:"attributions"`

	// Content is the HTML content of the article. It isn't included as YAML
	// frontmatter, and is rather split out of an article's Markdown file,
	// rendered, and then added separately.
	Content string `yaml:"-"`

	// Draft indicates that the article is not yet published.
	Draft bool `yaml:"-"`

	// HNLink is an optional link to comments on Hacker News.
	HNLink string `yaml:"hn_link"`

	// Hook is a leading sentence or two to succinctly introduce the article.
	Hook string `yaml:"hook"`

	// HookImageURL is the URL for a hook image for the article (to be shown on
	// the article index) if one was found.
	HookImageURL string `yaml:"-"`

	// Image is an optional image that may be included with an article.
	Image string `yaml:"image"`

	// Location is the geographical location where this article was written.
	Location string `yaml:"location"`

	// PublishedAt is when the article was published.
	PublishedAt *time.Time `yaml:"published_at"`

	// Slug is a unique identifier for the article that also helps determine
	// where it's addressable by URL.
	Slug string `yaml:"-"`

	// Tags are the set of tags that the article is tagged with.
	Tags []Tag `yaml:"tags"`

	// Title is the article's title.
	Title string `yaml:"title"`

	// TOC is the HTML rendered table of contents of the article. It isn't
	// included as YAML frontmatter, but rather calculated from the article's
	// content, rendered, and then added separately.
	TOC string `yaml:"-"`
}

// publishingInfo produces a brief spiel about publication which is intended to
// go into the left sidebar when an article is shown.
func (a *Article) publishingInfo() string {
	return `<p><strong>Article</strong><br>` + a.Title + `</p>` +
		`<p><strong>Published</strong><br>` + a.PublishedAt.Format("January 2, 2006") + `</p> ` +
		`<p><strong>Location</strong><br>` + a.Location + `</p>` +
		scommon.TwitterInfo
}

// taggedWith returns true if the given tag is in this article's set of tags
// and false otherwise.
func (a *Article) taggedWith(tag Tag) bool {
	for _, t := range a.Tags {
		if t == tag {
			return true
		}
	}

	return false
}

func (a *Article) validate(source string) error {
	if a.Location == "" {
		return fmt.Errorf("No location for article: %v", source)
	}

	if a.Title == "" {
		return fmt.Errorf("No title for article: %v", source)
	}

	if a.PublishedAt == nil {
		return fmt.Errorf("No publish date for article: %v", source)
	}

	return nil
}

// Fragment represents a fragment (that is, a short "stream of consciousness"
// style article) to be rendered.
type Fragment struct {
	// Attributions are any attributions for content that may be included in
	// the article (like an image in the header for example).
	Attributions string `yaml:"attributions"`

	// Content is the HTML content of the fragment. It isn't included as YAML
	// frontmatter, and is rather split out of an fragment's Markdown file,
	// rendered, and then added separately.
	Content string `yaml:"-"`

	// Draft indicates that the fragment is not yet published.
	Draft bool `yaml:"-"`

	// HNLink is an optional link to comments on Hacker News.
	HNLink string `yaml:"hn_link"`

	// Hook is a leading sentence or two to succinctly introduce the fragment.
	Hook string `yaml:"hook"`

	// Image is an optional image that may be included with a fragment.
	Image string `yaml:"image"`

	// Location is the geographical location where this article was written.
	Location string `yaml:"location"`

	// PublishedAt is when the fragment was published.
	PublishedAt *time.Time `yaml:"published_at"`

	// Slug is a unique identifier for the fragment that also helps determine
	// where it's addressable by URL.
	Slug string `yaml:"-"`

	// Title is the fragment's title.
	Title string `yaml:"title"`
}

// PublishingInfo produces a brief spiel about publication which is intended to
// go into the left sidebar when a fragment is shown.
func (f *Fragment) publishingInfo() string {
	s := `<p><strong>Fragment</strong><br>` + f.Title + `</p>` +
		`<p><strong>Published</strong><br>` + f.PublishedAt.Format("January 2, 2006") + `</p> `

	if f.Location != "" {
		s += `<p><strong>Location</strong><br>` + f.Location + `</p>`
	}

	s += scommon.TwitterInfo
	return s
}

func (f *Fragment) validate(source string) error {
	if f.Title == "" {
		return fmt.Errorf("No title for fragment: %v", source)
	}

	if f.PublishedAt == nil {
		return fmt.Errorf("No publish date for fragment: %v", source)
	}

	return nil
}

// Page is the metadata for a static HTML page generated from an ACE file.
// Currently the layouting system of ACE doesn't allow us to pass metadata up
// very well, so we have this instead.
type Page struct {
	// BodyClass is the CSS class that will be assigned to the body tag when
	// the page is rendered.
	BodyClass string `yaml:"body_class"`

	// Title is the HTML title that will be assigned to the page when it's
	// rendered.
	Title string `yaml:"title"`
}

// Photo is a photograph.
type Photo struct {
	// Description is the description of the photograph.
	Description string `yaml:"description"`

	// KeepInHomeRotation is a special override for photos I really like that
	// keeps them in the home page's random rotation. The rotation then
	// consists of either a recent photo or one of these explicitly selected
	// old ones.
	KeepInHomeRotation bool `yaml:"keep_in_home_rotation"`

	// OriginalImageURL is the location where the original-sized version of the
	// photo can be downloaded from.
	OriginalImageURL string `yaml:"original_image_url"`

	// OccurredAt is UTC time when the photo was published.
	OccurredAt *time.Time `yaml:"occurred_at"`

	// Slug is a unique identifier for the photo. Originally these were
	// generated from Flickr, but I've since just started reusing them for
	// filenames.
	Slug string `yaml:"slug"`

	// Title is the title of the photograph.
	Title string `yaml:"title"`
}

// PhotoWrapper is a data structure intended to represent the data structure at
// the top level of photograph data file `content/photographs/_meta.yaml`.
type PhotoWrapper struct {
	// Photos is a collection of photos within the top-level wrapper.
	Photos []*Photo `yaml:"photographs"`
}

// Tag is a symbol assigned to an article to categorize it.
//
// This feature is not meanted to be overused. It's really just for tagging
// a few particular things so that we can generate content-specific feeds for
// certain aggregates (so far just Planet Postgres).
type Tag string

// articleYear holds a collection of articles grouped by year.
type articleYear struct {
	Year     int
	Articles []*Article
}

// fragmentYear holds a collection of fragments grouped by year.
type fragmentYear struct {
	Year      int
	Fragments []*Fragment
}

// twitterCard represents a Twitter "card" (i.e. one of those rich media boxes
// that sometimes appear under tweets official clients) for use in templates.
type twitterCard struct {
	// Description is the title to show in the card.
	Title string

	// Description is the description to show in the card.
	Description string

	// ImageURL is the URL to the image to show in the card. It should be
	// absolute because Twitter will need to be able to fetch it from our
	// servers. Leave blank if there is no image.
	ImageURL string
}

//////////////////////////////////////////////////////////////////////////////
//
//
//
// Private
//
//
//
//////////////////////////////////////////////////////////////////////////////

func compileJavascripts(c *modulir.Context, versionedAssetsDir string) (bool, error) {
	sourceDir := c.SourceDir + "/content/javascripts"

	sources, err := mfile.ReadDir(c, sourceDir)
	if err != nil {
		return false, err
	}

	sourcesChanged := c.ChangedAny(sources...)
	if !sourcesChanged && !c.Forced() {
		return false, nil
	}

	return true, sassets.CompileJavascripts(
		c,
		sourceDir,
		versionedAssetsDir+"/app.js")
}

func compileStylesheets(c *modulir.Context, versionedAssetsDir string) (bool, error) {
	sourceDir := c.SourceDir + "/content/stylesheets"

	sources, err := mfile.ReadDir(c, sourceDir)
	if err != nil {
		return false, err
	}

	sourcesChanged := c.ChangedAny(sources...)
	if !sourcesChanged && !c.Forced() {
		return false, nil
	}

	return true, sassets.CompileStylesheets(
		c,
		sourceDir,
		versionedAssetsDir+"/app.css")
}

// extractSlug gets a slug for the given filename by using its basename
// stripped of file extension.
func extractSlug(source string) string {
	return strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
}

func fetchAndResizePhoto(c *modulir.Context, dir string, photo *Photo) (bool, error) {
	// source without an extension, e.g. `content/photographs/123`
	sourceNoExt := filepath.Join(dir, photo.Slug)

	// A "marker" is an empty file that we commit to a photograph directory
	// that indicates that we've already done the work to fetch and resize a
	// photo. It allows us to skip duplicate work even if we don't have the
	// work's results available locally. This is important for CI where we
	// store results to an S3 bucket, but don't pull them all back down again
	// for every build.
	markerPath := sourceNoExt + ".marker"

	// We use an in-memory cache to store whether markers exist for some period
	// of time because going to the filesystem to check every one of them is
	// relatively slow/expensive.
	if _, ok := photoMarkerCache.Get(markerPath); ok {
		c.Log.Debugf("Skipping photo fetch + resize because marker cached: %s",
			markerPath)
		return false, nil
	}

	// Otherwise check the filesystem.
	if mfile.Exists(markerPath) {
		c.Log.Debugf("Skipping photo fetch + resize because marker exists: %s",
			markerPath)
		photoMarkerCache.Set(markerPath, struct{}{}, gocache.DefaultExpiration)
		return false, nil
	}

	originalPath := filepath.Join(scommon.TempDir, photo.Slug+"_original.jpg")

	err := fetchURL(c, photo.OriginalImageURL, originalPath)
	if err != nil {
		return true, errors.Wrapf(err, "Error fetching photograph: %s", photo.Slug)
	}

	resizeMatrix := []struct {
		Target string
		Width  int
	}{
		{sourceNoExt + ".jpg", 333},
		{sourceNoExt + "@2x.jpg", 667},
		{sourceNoExt + "_large.jpg", 1500},
		{sourceNoExt + "_large@2x.jpg", 3000},
	}
	for _, resize := range resizeMatrix {
		err := resizeImage(c, originalPath, resize.Target, resize.Width)
		if err != nil {
			return true, errors.Wrapf(err, "Error resizing photograph: %s", photo.Slug)
		}
	}

	// After everything is done, created a marker file to indicate that the
	// work doesn't need to be redone.
	file, err := os.OpenFile(markerPath, os.O_RDONLY|os.O_CREATE, 0755)
	if err != nil {
		return true, errors.Wrapf(err, "Error creating marker for photograph: %s", photo.Slug)
	}
	file.Close()

	return true, nil
}

// fetchURL is a helper for fetching a file via HTTP and storing it the local
// filesystem.
func fetchURL(c *modulir.Context, source, target string) error {
	c.Log.Debugf("Fetching file: %v", source)

	resp, err := http.Get(source)
	if err != nil {
		return errors.Wrapf(err, "Error fetching: %v", source)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("Unexpected status code fetching '%v': %d",
			source, resp.StatusCode)
	}

	f, err := os.Create(target)
	if err != nil {
		return errors.Wrapf(err, "Error creating: %v", target)
	}
	defer f.Close()

	w := bufio.NewWriter(f)

	// probably not needed
	defer w.Flush()

	_, err = io.Copy(w, resp.Body)
	if err != nil {
		return errors.Wrapf(err, "Error copying to '%v' from HTTP response",
			target)
	}

	return nil
}

// Gets a map of local values for use while rendering a template and includes
// a few "special" values that are globally relevant to all templates.
func getLocals(title string, locals map[string]interface{}) map[string]interface{} {
	defaults := map[string]interface{}{
		"BodyClass":         "",
		"GoogleAnalyticsID": conf.GoogleAnalyticsID,
		"LocalFonts":        conf.LocalFonts,
		"Release":           Release,
		"Title":             title,
		"TwitterCard":       nil,
		"ViewportWidth":     "device-width",
	}

	for k, v := range locals {
		defaults[k] = v
	}

	return defaults
}

func groupArticlesByYear(articles []*Article) []*articleYear {
	var year *articleYear
	var years []*articleYear

	for _, article := range articles {
		if year == nil || year.Year != article.PublishedAt.Year() {
			year = &articleYear{article.PublishedAt.Year(), nil}
			years = append(years, year)
		}

		year.Articles = append(year.Articles, article)
	}

	return years
}

func groupFragmentsByYear(fragments []*Fragment) []*fragmentYear {
	var year *fragmentYear
	var years []*fragmentYear

	for _, fragment := range fragments {
		if year == nil || year.Year != fragment.PublishedAt.Year() {
			year = &fragmentYear{fragment.PublishedAt.Year(), nil}
			years = append(years, year)
		}

		year.Fragments = append(year.Fragments, fragment)
	}

	return years
}

func insertOrReplaceArticle(articles *[]*Article, article *Article) {
	for i, a := range *articles {
		if article.Slug == a.Slug {
			(*articles)[i] = article
			return
		}
	}

	*articles = append(*articles, article)
}

func insertOrReplaceFragment(fragments *[]*Fragment, fragment *Fragment) {
	for i, f := range *fragments {
		if fragment.Slug == f.Slug {
			(*fragments)[i] = fragment
			return
		}
	}

	*fragments = append(*fragments, fragment)
}

func insertOrReplacePassage(passages *[]*spassages.Passage, passage *spassages.Passage) {
	for i, p := range *passages {
		if passage.Slug == p.Slug {
			(*passages)[i] = passage
			return
		}
	}

	*passages = append(*passages, passage)
}

func insertOrReplaceTalk(talks *[]*stalks.Talk, talk *stalks.Talk) {
	for i, t := range *talks {
		if talk.Slug == t.Slug {
			(*talks)[i] = talk
			return
		}
	}

	*talks = append(*talks, talk)
}

// isDraft does really simplistic detection on whether the given source is a
// draft by looking whether the name "drafts" is in its parent directory's
// name.
func isDraft(source string) bool {
	return strings.Contains(filepath.Base(filepath.Dir(source)), "drafts")
}

// Checks if the path exists as a common image format (.jpg or .png only). If
// so, returns the discovered extension (e.g. "jpg") and boolean true.
// Otherwise returns an empty string and boolean false.
func pathAsImage(extensionlessPath string) (string, bool) {
	// extensions must be lowercased
	formats := []string{"jpg", "png"}

	for _, format := range formats {
		_, err := os.Stat(extensionlessPath + "." + format)
		if err != nil {
			continue
		}

		return format, true
	}

	return "", false
}

func renderArticle(c *modulir.Context, source string, articles *[]*Article, articlesChanged *bool, mu *sync.Mutex) (bool, error) {
	sourceChanged := c.Changed(source)
	viewsChanged := c.ChangedAny(append(
		[]string{
			scommon.MainLayout,
			scommon.ViewsDir + "/articles/show.ace",
		},
		partialViews...,
	)...)
	if !sourceChanged && !viewsChanged && !c.Forced() {
		return false, nil
	}

	var article Article
	data, err := myaml.ParseFileFrontmatter(c, source, &article)
	if err != nil {
		return true, err
	}

	err = article.validate(source)
	if err != nil {
		return true, err
	}

	article.Draft = isDraft(source)
	article.Slug = extractSlug(source)

	article.Content = smarkdown.Render(string(data), nil)

	article.TOC, err = mtoc.RenderFromHTML(article.Content)
	if err != nil {
		return true, err
	}

	format, ok := pathAsImage(
		path.Join(c.SourceDir, "content", "images", article.Slug, "hook"),
	)
	if ok {
		article.HookImageURL = "/assets/images/" + article.Slug + "/hook." + format
	}

	card := &twitterCard{
		Title:       article.Title,
		Description: article.Hook,
	}
	format, ok = pathAsImage(
		path.Join(c.SourceDir, "content", "images", article.Slug, "twitter@2x"),
	)
	if ok {
		card.ImageURL = conf.AbsoluteURL + "/assets/images/" + article.Slug + "/twitter@2x." + format
	}

	locals := getLocals(article.Title, map[string]interface{}{
		"Article":        article,
		"PublishingInfo": article.publishingInfo(),
		"TwitterCard":    card,
	})

	// Always use force context because if we made it to here we know that our
	// sources have changed.
	err = mace.RenderFile(c, scommon.MainLayout, scommon.ViewsDir+"/articles/show.ace",
		path.Join(c.TargetDir, article.Slug), stemplate.GetAceOptions(viewsChanged), locals)
	if err != nil {
		return true, err
	}

	mu.Lock()
	insertOrReplaceArticle(articles, &article)
	*articlesChanged = true
	mu.Unlock()

	return true, nil
}

func renderArticlesIndex(c *modulir.Context, articles []*Article, articlesChanged bool) (bool, error) {
	viewsChanged := c.ChangedAny(append(
		[]string{
			scommon.MainLayout,
			scommon.ViewsDir + "/articles/index.ace",
		},
		partialViews...,
	)...)
	if !articlesChanged && !viewsChanged && !c.Forced() {
		return false, nil
	}

	articlesByYear := groupArticlesByYear(articles)

	locals := getLocals("Articles", map[string]interface{}{
		"ArticlesByYear": articlesByYear,
	})

	return true, mace.RenderFile(c, scommon.MainLayout, scommon.ViewsDir+"/articles/index.ace",
		c.TargetDir+"/articles/index.html", stemplate.GetAceOptions(viewsChanged), locals)
}

func renderArticlesFeed(c *modulir.Context, articles []*Article, tag *Tag, articlesChanged bool) (bool, error) {
	if !articlesChanged && !c.Forced() {
		return false, nil
	}

	name := "articles"
	if tag != nil {
		name = fmt.Sprintf("articles-%s", *tag)
	}
	filename := name + ".atom"

	title := "Articles - brandur.org"
	if tag != nil {
		title = fmt.Sprintf("Articles (%s) - brandur.org", *tag)
	}

	feed := &satom.Feed{
		Title: title,
		ID:    "tag:brandur.org.org,2013:/" + name,

		Links: []*satom.Link{
			{Rel: "self", Type: "application/atom+xml", Href: "https://brandur.org/" + filename},
			{Rel: "alternate", Type: "text/html", Href: "https://brandur.org"},
		},
	}

	if len(articles) > 0 {
		feed.Updated = *articles[0].PublishedAt
	}

	for i, article := range articles {
		if tag != nil && !article.taggedWith(*tag) {
			continue
		}

		if i >= conf.NumAtomEntries {
			break
		}

		entry := &satom.Entry{
			Title:     article.Title,
			Content:   &satom.EntryContent{Content: article.Content, Type: "html"},
			Published: *article.PublishedAt,
			Updated:   *article.PublishedAt,
			Link:      &satom.Link{Href: conf.AbsoluteURL + "/" + article.Slug},
			ID:        "tag:brandur.org," + article.PublishedAt.Format("2006-01-02") + ":" + article.Slug,

			AuthorName: scommon.AtomAuthorName,
			AuthorURI:  conf.AbsoluteURL,
		}
		feed.Entries = append(feed.Entries, entry)
	}

	f, err := os.Create(path.Join(conf.TargetDir, filename))
	if err != nil {
		return true, err
	}
	defer f.Close()

	return true, feed.Encode(f, "  ")
}

func renderFragment(c *modulir.Context, source string, fragments *[]*Fragment, fragmentsChanged *bool, mu *sync.Mutex) (bool, error) {
	sourceChanged := c.Changed(source)
	viewsChanged := c.ChangedAny(append(
		[]string{
			scommon.MainLayout,
			scommon.ViewsDir + "/fragments/show.ace",
		},
		partialViews...,
	)...)
	if !sourceChanged && !viewsChanged && !c.Forced() {
		return false, nil
	}

	var fragment Fragment
	data, err := myaml.ParseFileFrontmatter(c, source, &fragment)
	if err != nil {
		return true, err
	}

	err = fragment.validate(source)
	if err != nil {
		return true, err
	}

	fragment.Draft = isDraft(source)
	fragment.Slug = extractSlug(source)

	fragment.Content = smarkdown.Render(string(data), nil)

	card := &twitterCard{
		Title:       fragment.Title,
		Description: fragment.Hook,
	}
	format, ok := pathAsImage(
		path.Join(c.SourceDir, "content", "images", "fragments", fragment.Slug, "twitter@2x"),
	)
	if ok {
		card.ImageURL = conf.AbsoluteURL + "/assets/images/fragments/" + fragment.Slug + "/twitter@2x." + format
	}

	locals := getLocals(fragment.Title, map[string]interface{}{
		"Fragment":       fragment,
		"PublishingInfo": fragment.publishingInfo(),
		"TwitterCard":    card,
	})

	err = mace.RenderFile(c, scommon.MainLayout, scommon.ViewsDir+"/fragments/show.ace",
		path.Join(c.TargetDir, "fragments", fragment.Slug),
		stemplate.GetAceOptions(viewsChanged), locals)
	if err != nil {
		return true, err
	}

	mu.Lock()
	insertOrReplaceFragment(fragments, &fragment)
	*fragmentsChanged = true
	mu.Unlock()

	return true, nil
}

func renderFragmentsFeed(c *modulir.Context, fragments []*Fragment,
	fragmentsChanged bool) (bool, error) {
	if !fragmentsChanged && !c.Forced() {
		return false, nil
	}

	feed := &satom.Feed{
		Title: "Fragments - brandur.org",
		ID:    "tag:brandur.org.org,2013:/fragments",

		Links: []*satom.Link{
			{Rel: "self", Type: "application/atom+xml", Href: "https://brandur.org/fragments.atom"},
			{Rel: "alternate", Type: "text/html", Href: "https://brandur.org"},
		},
	}

	if len(fragments) > 0 {
		feed.Updated = *fragments[0].PublishedAt
	}

	for i, fragment := range fragments {
		if i >= conf.NumAtomEntries {
			break
		}

		entry := &satom.Entry{
			Title:     fragment.Title,
			Content:   &satom.EntryContent{Content: fragment.Content, Type: "html"},
			Published: *fragment.PublishedAt,
			Updated:   *fragment.PublishedAt,
			Link:      &satom.Link{Href: conf.AbsoluteURL + "/fragments/" + fragment.Slug},
			ID:        "tag:brandur.org," + fragment.PublishedAt.Format("2006-01-02") + ":fragments/" + fragment.Slug,

			AuthorName: scommon.AtomAuthorName,
			AuthorURI:  conf.AbsoluteURL,
		}
		feed.Entries = append(feed.Entries, entry)
	}

	f, err := os.Create(conf.TargetDir + "/fragments.atom")
	if err != nil {
		return true, err
	}
	defer f.Close()

	return true, feed.Encode(f, "  ")
}

func renderFragmentsIndex(c *modulir.Context, fragments []*Fragment,
	fragmentsChanged bool) (bool, error) {
	viewsChanged := c.ChangedAny(append(
		[]string{
			scommon.MainLayout,
			scommon.ViewsDir + "/fragments/show.ace",
		},
		partialViews...,
	)...)
	if !fragmentsChanged && !viewsChanged && !c.Forced() {
		return false, nil
	}

	fragmentsByYear := groupFragmentsByYear(fragments)

	locals := getLocals("Fragments", map[string]interface{}{
		"FragmentsByYear": fragmentsByYear,
	})

	return true, mace.RenderFile(c, scommon.MainLayout, scommon.ViewsDir+"/fragments/index.ace",
		c.TargetDir+"/fragments/index.html", stemplate.GetAceOptions(viewsChanged), locals)
}

func renderPassage(c *modulir.Context, source string, passages *[]*spassages.Passage, passagesChanged *bool, mu *sync.Mutex) (bool, error) {
	sourceChanged := c.Changed(source)
	viewsChanged := c.ChangedAny(append(
		[]string{
			scommon.PassageLayout,
			scommon.ViewsDir + "/passages/show.ace",
		},
		partialViews...,
	)...)
	if !sourceChanged && !viewsChanged && !c.Forced() {
		return false, nil
	}

	// TODO: modulir-ize this package
	passage, err := spassages.Render(c, filepath.Dir(source), filepath.Base(source),
		conf.AbsoluteURL, false)
	if err != nil {
		return true, err
	}

	locals := getLocals(passage.Title, map[string]interface{}{
		"InEmail": false,
		"Passage": passage,
	})

	err = mace.RenderFile(c, scommon.PassageLayout, scommon.ViewsDir+"/passages/show.ace",
		c.TargetDir+"/passages/"+passage.Slug, stemplate.GetAceOptions(viewsChanged), locals)
	if err != nil {
		return true, err
	}

	mu.Lock()
	insertOrReplacePassage(passages, passage)
	*passagesChanged = true
	mu.Unlock()

	return true, nil
}

func renderPassagesIndex(c *modulir.Context, passages []*spassages.Passage,
	passagesChanged bool) (bool, error) {
	viewsChanged := c.ChangedAny(append(
		[]string{
			scommon.PassageLayout,
			scommon.ViewsDir + "/passages/index.ace",
		},
		partialViews...,
	)...)
	if !passagesChanged && !viewsChanged && !c.Forced() {
		return false, nil
	}

	locals := getLocals("Passages", map[string]interface{}{
		"Passages": passages,
	})

	return true, mace.RenderFile(c, scommon.PassageLayout, scommon.ViewsDir+"/passages/index.ace",
		c.TargetDir+"/passages/index.html", stemplate.GetAceOptions(viewsChanged), locals)
}

func renderHome(c *modulir.Context,
	articles []*Article, fragments []*Fragment, photos []*Photo,
	articlesChanged, fragmentsChanged, photosChanged bool) (bool, error) {

	viewsChanged := c.ChangedAny(append(
		[]string{
			scommon.MainLayout,
			scommon.ViewsDir + "/index.ace",
		},
		partialViews...,
	)...)
	if !articlesChanged && !fragmentsChanged && !photosChanged && !viewsChanged && !c.Forced() {
		return false, nil
	}

	if len(articles) > 3 {
		articles = articles[0:3]
	}

	// Try just one fragment for now to better balance the page's height.
	if len(fragments) > 1 {
		fragments = fragments[0:1]
	}

	// Find a random photo to put on the homepage.
	photo := selectRandomPhoto(photos)

	locals := getLocals("brandur.org", map[string]interface{}{
		"Articles":  articles,
		"BodyClass": "index",
		"Fragments": fragments,
		"Photo":     photo,
	})

	return true, mace.RenderFile(c, scommon.MainLayout, scommon.ViewsDir+"/index.ace",
		c.TargetDir+"/index.html", stemplate.GetAceOptions(viewsChanged), locals)
}

func renderPage(c *modulir.Context, source string, meta map[string]*Page, metaChanged bool) (bool, error) {
	viewsChanged := c.ChangedAny(append(
		[]string{
			scommon.MainLayout,
			source,
		},
		partialViews...,
	)...)
	if !metaChanged && !viewsChanged && !c.Forced() {
		return false, nil
	}

	// Remove the "./pages" directory and extension, but keep the rest of the
	// path.
	//
	// Looks something like "about", or "nested/about".
	pagePath := strings.TrimPrefix(mfile.MustAbs(source),
		mfile.MustAbs("./pages")+"/")
	pagePath = strings.TrimSuffix(pagePath, path.Ext(pagePath))

	// Looks something like "./public/about".
	target := path.Join(c.TargetDir, pagePath)

	// Put a ".html" on if this page is an index. This will allow our local
	// server to serve it at a directory path, and our upload script is smart
	// enough to do the right thing with it as well.
	if path.Base(pagePath) == "index" {
		target += ".html"
	}

	locals := map[string]interface{}{
		"BodyClass": "",
		"Title":     "Untitled Page",
	}

	pageMeta, ok := meta[pagePath]
	if ok {
		locals = map[string]interface{}{
			"BodyClass": pageMeta.BodyClass,
			"Title":     pageMeta.Title,
		}
	} else {
		c.Log.Errorf("No page meta information: %v", pagePath)
	}

	locals = getLocals("Page", locals)

	err := mfile.EnsureDir(c, path.Dir(target))
	if err != nil {
		return true, err
	}

	err = mace.RenderFile(c, scommon.MainLayout, source, target,
		stemplate.GetAceOptions(viewsChanged), locals)
	return true, nil
}

func renderReading(c *modulir.Context, db *sql.DB) (bool, error) {
	if db == nil {
		return false, nil
	}

	viewsChanged := c.ChangedAny(append(
		[]string{
			scommon.MainLayout,
			scommon.ViewsDir + "/reading/index.ace",
		},
		partialViews...,
	)...)
	if !c.FirstRun && !viewsChanged && !c.Forced() {
		return false, nil
	}

	return true, squantified.RenderReading(c, db, viewsChanged, getLocals)
}

func renderPhotoIndex(c *modulir.Context, photos []*Photo,
	photosChanged bool) (bool, error) {
	viewsChanged := c.ChangedAny(append(
		[]string{
			scommon.MainLayout,
			scommon.ViewsDir + "/photos/index.ace",
		},
		partialViews...,
	)...)
	if !photosChanged && !viewsChanged && !c.Forced() {
		return false, nil
	}

	locals := getLocals("Photos", map[string]interface{}{
		"BodyClass":     "photos",
		"Photos":        photos,
		"ViewportWidth": 600,
	})

	return true, mace.RenderFile(c, scommon.MainLayout, scommon.ViewsDir+"/photos/index.ace",
		c.TargetDir+"/photos/index.html", stemplate.GetAceOptions(viewsChanged), locals)
}

func renderRobotsTxt(c *modulir.Context) (bool, error) {
	if !c.FirstRun && !c.Forced() {
		return false, nil
	}

	var content string
	if conf.Drafts {
		// Allow Twitterbot so that we can preview card images on dev.
		content = `User-agent: Twitterbot
Disallow:

User-agent: *
Disallow: /
`
	} else {
		// Disallow acccess to photos because the content isn't very
		// interesting for robots and they're bandwidth heavy.
		content = `User-agent: *
Disallow: /photographs/
Disallow: /photos
`
	}

	outFile, err := os.Create(c.TargetDir + "/robots.txt")
	if err != nil {
		return true, err
	}
	outFile.WriteString(content)
	outFile.Close()

	return true, nil
}

func renderRuns(c *modulir.Context, db *sql.DB) (bool, error) {
	if db == nil {
		return false, nil
	}

	viewsChanged := c.ChangedAny(append(
		[]string{
			scommon.MainLayout,
			scommon.ViewsDir + "/runs/index.ace",
		},
		partialViews...,
	)...)
	if !c.FirstRun && !viewsChanged && !c.Forced() {
		return false, nil
	}

	return true, squantified.RenderRuns(c, db, viewsChanged, getLocals)
}

func renderSequence(c *modulir.Context, sequenceName string, photo *Photo,
	sequenceChanged bool) (bool, error) {
	viewsChanged := c.ChangedAny(append(
		[]string{
			scommon.MainLayout,
			scommon.ViewsDir + "/sequences/photo.ace",
		},
		partialViews...,
	)...)
	if !sequenceChanged && !viewsChanged && !c.Forced() {
		return false, nil
	}

	title := fmt.Sprintf("%s — %s", photo.Title, sequenceName)
	description := string(mmarkdown.Render(c, []byte(photo.Description)))

	locals := getLocals(title, map[string]interface{}{
		"BodyClass":     "sequences-photo",
		"Description":   description,
		"Photo":         photo,
		"SequenceName":  sequenceName,
		"ViewportWidth": 600,
	})

	return true, mace.RenderFile(c, scommon.MainLayout, scommon.ViewsDir+"/sequences/photo.ace",
		path.Join(c.TargetDir, "sequences", sequenceName, photo.Slug),
		stemplate.GetAceOptions(viewsChanged), locals)
}

func renderTalk(c *modulir.Context, source string, talks *[]*stalks.Talk, talksChanged *bool, mu *sync.Mutex) (bool, error) {
	sourceChanged := c.Changed(source)
	viewsChanged := c.ChangedAny(append(
		[]string{
			scommon.MainLayout,
			scommon.ViewsDir + "/talks/show.ace",
		},
		partialViews...,
	)...)
	if !sourceChanged && !viewsChanged && !c.Forced() {
		return false, nil
	}

	// TODO: modulir-ize this package
	talk, err := stalks.Render(
		c, c.SourceDir+"/content", filepath.Dir(source), filepath.Base(source))
	if err != nil {
		return true, err
	}

	locals := getLocals(talk.Title, map[string]interface{}{
		"BodyClass":      "talk",
		"PublishingInfo": talk.PublishingInfo(),
		"Talk":           talk,
	})

	err = mace.RenderFile(c, scommon.MainLayout, scommon.ViewsDir+"/talks/show.ace",
		path.Join(c.TargetDir, talk.Slug), stemplate.GetAceOptions(viewsChanged), locals)
	if err != nil {
		return true, err
	}

	mu.Lock()
	insertOrReplaceTalk(talks, talk)
	*talksChanged = true
	mu.Unlock()

	return true, nil
}

func renderTwitter(c *modulir.Context, db *sql.DB) (bool, error) {
	if db == nil {
		return false, nil
	}

	viewsChanged := c.ChangedAny(append(
		[]string{
			scommon.MainLayout,
			scommon.ViewsDir + "/twitter/index.ace",
		},
		partialViews...,
	)...)
	if !c.FirstRun && !viewsChanged && !c.Forced() {
		return false, nil
	}

	return true, squantified.RenderTwitter(c, db, viewsChanged, getLocals)
}

func resizeImage(c *modulir.Context, source, target string, width int) error {
	cmd := exec.Command(
		"gm",
		"convert",
		source,
		"-auto-orient",
		"-resize",
		fmt.Sprintf("%vx", width),
		"-quality",
		"85",
		target,
	)

	var errOut bytes.Buffer
	cmd.Stderr = &errOut

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("%v (stderr: %v)", err, errOut.String())
	}

	return nil
}

func selectRandomPhoto(photos []*Photo) *Photo {
	if len(photos) < 1 {
		return nil
	}

	numRecent := 20
	if len(photos) < numRecent {
		numRecent = len(photos)
	}

	// All recent photos go into the random selection.
	randomPhotos := photos[0:numRecent]

	// Older photos that are good enough that I've explicitly tagged them
	// as such also get considered for the rotation.
	if len(photos) > numRecent {
		olderPhotos := photos[numRecent : len(photos)-1]

		for _, photo := range olderPhotos {
			if photo.KeepInHomeRotation {
				randomPhotos = append(randomPhotos, photo)
			}
		}
	}

	return randomPhotos[rand.Intn(len(randomPhotos))]
}

func sortArticles(articles []*Article) {
	sort.Slice(articles, func(i, j int) bool {
		return articles[j].PublishedAt.Before(*articles[i].PublishedAt)
	})
}

func sortFragments(fragments []*Fragment) {
	sort.Slice(fragments, func(i, j int) bool {
		return fragments[j].PublishedAt.Before(*fragments[i].PublishedAt)
	})
}

func sortPassages(passages []*spassages.Passage) {
	sort.Slice(passages, func(i, j int) bool {
		return passages[j].PublishedAt.Before(*passages[i].PublishedAt)
	})
}

func sortPhotos(photos []*Photo) {
	sort.Slice(photos, func(i, j int) bool {
		return photos[j].OccurredAt.Before(*photos[i].OccurredAt)
	})
}

func sortTalks(talks []*stalks.Talk) {
	sort.Slice(talks, func(i, j int) bool {
		return talks[j].PublishedAt.Before(*talks[i].PublishedAt)
	})
}

// Gets a pointer to a tag just to work around the fact that you can take the
// address of a constant like `tagPostgres`.
func tagPointer(tag Tag) *Tag {
	return &tag
}
