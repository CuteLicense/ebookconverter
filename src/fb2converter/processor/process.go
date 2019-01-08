package processor

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"io"
	"io/ioutil"
	"math/rand"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/asaskevich/govalidator"
	"github.com/beevik/etree"
	"github.com/google/uuid"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
	"gopkg.in/gomail.v2"

	"fb2converter/config"
	"fb2converter/state"
)

// Various directories used across the program
const (
	DirContent    = "OEBPS"
	DirMata       = "META-INF"
	DirImages     = "images"
	DirFonts      = "fonts"
	DirVignettes  = "vignettes"
	DirProfile    = "profiles"
	DirHyphenator = "dictionaries"
	DirResources  = "resources"
)

// will be used to derive UUIDs from non-parsable book ID
var nameSpaceFB2 = uuid.MustParse("09aa0c17-ca72-42d3-afef-75911e5d7646")

// Processor state.
type Processor struct {
	// input parameters
	src string
	dst string
	// parameters translated to internal types
	nodirs         bool
	stk            bool
	format         OutputFmt
	notesMode      NotesFmt
	tocPlacement   TOCPlacement
	tocType        TOCType
	kindlePageMap  APNXGeneration
	stampPlacement StampPlacement
	// working directory
	tmpDir string
	// input document
	doc *etree.Document
	// parsing state and conversion results
	Book     *Book
	notFound *binary
	// program environment
	env             *state.LocalEnv
	speechTransform *config.Transformation
	dashTransform   *config.Transformation
	kindlegenPath   string
}

// New creates book processor and prepares necessary temporary directories.
func New(r io.Reader, unknownEncoding bool, src, dst string, nodirs, stk bool, format OutputFmt, env *state.LocalEnv) (*Processor, error) {

	kindle := format == OAzw3 || format == OMobi

	u, err := uuid.NewRandom()
	if err != nil {
		return nil, errors.Wrap(err, "unable to generate UUID")
	}

	notes := ParseNotesString(env.Cfg.Doc.Notes.Mode)
	if notes == UnsupportedNotesFmt {
		env.Log.Warn("Unknown notes mode requested, switching to default", zap.String("mode", env.Cfg.Doc.Notes.Mode))
		notes = NDefault
	}
	toct := ParseTOCTypeString(env.Cfg.Doc.TOC.Type)
	if toct == UnsupportedTOCType {
		env.Log.Warn("Unknown TOC type requested, switching to normal", zap.String("type", env.Cfg.Doc.TOC.Type))
		toct = TOCTypeNormal
	}
	place := ParseTOCPlacementString(env.Cfg.Doc.TOC.Placement)
	if place == UnsupportedTOCPlacement {
		env.Log.Warn("Unknown TOC page placement requested, turning off generation", zap.String("placement", env.Cfg.Doc.TOC.Placement))
		place = TOCNone
	}
	var apnx APNXGeneration
	if kindle {
		if stk && format == OMobi && env.Cfg.SMTPConfig.IsValid() && env.Cfg.SMTPConfig.DeleteOnSuccess {
			// Do not create pagemap - we do not need it
			apnx = APNXNone
		} else {
			apnx = ParseAPNXGenerationSring(env.Cfg.Doc.Kindlegen.PageMap)
			if apnx == UnsupportedAPNXGeneration {
				env.Log.Warn("Unknown APNX generation option requested, turning off", zap.String("apnx", env.Cfg.Doc.Kindlegen.PageMap))
				apnx = APNXNone
			}
		}
	}
	var stamp StampPlacement
	if len(env.Cfg.Doc.Cover.Placement) > 0 {
		stamp = ParseStampPlacementString(env.Cfg.Doc.Cover.Placement)
		if stamp == UnsupportedStampPlacement {
			env.Log.Warn("Unknown stamp placement requested, using default (none - if book has cover, middle - otherwise)", zap.String("placement", env.Cfg.Doc.Cover.Placement))
		}
	}

	p := &Processor{
		src:             src,
		dst:             dst,
		nodirs:          nodirs,
		stk:             stk,
		format:          format,
		notesMode:       notes,
		tocType:         toct,
		tocPlacement:    place,
		kindlePageMap:   apnx,
		stampPlacement:  stamp,
		doc:             etree.NewDocument(),
		Book:            NewBook(u, filepath.Base(src)),
		env:             env,
		speechTransform: env.Cfg.GetTransformation("speech"),
		dashTransform:   env.Cfg.GetTransformation("dashes"),
	}
	p.doc.WriteSettings = etree.WriteSettings{CanonicalText: true, CanonicalAttrVal: true}

	if kindle {
		// Fail early
		if p.kindlegenPath, err = env.Cfg.GetKindlegenPath(); err != nil {
			return nil, err
		}
	}

	// sanity checking
	if p.speechTransform != nil && len(p.speechTransform.To) == 0 {
		env.Log.Warn("Invalid direct speech transformation, ignoring")
		p.speechTransform = nil
	}
	if p.dashTransform != nil && len(p.dashTransform.To) == 0 {
		env.Log.Warn("Invalid dash transformation, ignoring")
		p.dashTransform = nil
	}
	if p.dashTransform != nil {
		sym, _ := utf8.DecodeRuneInString(p.dashTransform.To)
		p.dashTransform.To = string(sym)
	}

	// re-route temporary directory for debugging
	if env.Debug {
		wd, err := os.Getwd()
		if err != nil {
			return nil, errors.Wrap(err, "unable to get working directory")
		}
		tmpd := filepath.Join(wd, "fb2c_deb")
		if err = os.MkdirAll(tmpd, 0700); err != nil {
			return nil, errors.Wrap(err, "unable to create debug directory")
		}
		t := time.Now()
		ulid, err := ulid.New(ulid.Timestamp(t), ulid.Monotonic(rand.New(rand.NewSource(t.UnixNano())), 0))
		if err != nil {
			return nil, errors.Wrap(err, "unable to allocate ULID")
		}
		p.tmpDir = filepath.Join(tmpd, ulid.String()+"_"+filepath.Base(src))
		if err = os.MkdirAll(p.tmpDir, 0700); err != nil {
			return nil, errors.Wrap(err, "unable to create temporary directory")
		}
	} else {
		p.tmpDir, err = ioutil.TempDir("", "fb2c-")
		if err != nil {
			return nil, errors.Wrap(err, "unable to create temporary directory")
		}
	}

	if unknownEncoding {
		// input file had no BOM mark - most likely was not Unicode
		p.doc.ReadSettings = etree.ReadSettings{
			CharsetReader: charset.NewReaderLabel,
		}
	}

	// Read and parse fb2
	if _, err := p.doc.ReadFrom(r); err != nil {
		return nil, errors.Wrap(err, "unable to parse FB2")
	}

	// Save parsed document back to file (pretty-printed) for debugging
	if p.env.Debug {
		p.doc.IndentTabs()
		if err := p.doc.WriteToFile(filepath.Join(p.tmpDir, filepath.Base(src))); err != nil {
			return nil, errors.Wrap(err, "unable to write XML")
		}
	}

	// we are ready to convert document
	return p, nil
}

// Process does all the work.
func (p *Processor) Process() error {

	// Processing - order of steps and their presence are important as information and context
	// being built and accumulated...

	if err := p.processNotes(); err != nil {
		return err
	}
	if err := p.processBinaries(); err != nil {
		return err
	}
	if err := p.processDescription(); err != nil {
		return err
	}
	if err := p.processBodies(); err != nil {
		return err
	}
	if err := p.processLinks(); err != nil {
		return err
	}
	if err := p.processImages(); err != nil {
		return err
	}
	if err := p.generateTOCPage(); err != nil {
		return err
	}
	if err := p.generateCover(); err != nil {
		return err
	}
	if err := p.generateNCX(); err != nil {
		return err
	}
	if err := p.prepareStylesheet(); err != nil {
		return err
	}
	if err := p.generatePagemap(); err != nil {
		return err
	}
	if err := p.generateOPF(); err != nil {
		return err
	}
	if err := p.generateMeta(); err != nil {
		return err
	}
	return nil
}

// Save makes the conversion results permanent by storing everything properly and cleaning temporary artifacts.
func (p *Processor) Save() (string, error) {

	start := time.Now()
	p.env.Log.Debug("Saving content - starting",
		zap.String("tmp", p.tmpDir),
		zap.String("content", DirContent),
	)
	defer func(start time.Time) {
		p.env.Log.Debug("Saving content - done", zap.Duration("elapsed", time.Now().Sub(start)))
	}(start)

	if err := p.Book.flushData(p.tmpDir); err != nil {
		return "", err
	}
	if err := p.Book.flushVignettes(p.tmpDir); err != nil {
		return "", err
	}
	if err := p.Book.flushImages(p.tmpDir); err != nil {
		return "", err
	}
	if err := p.Book.flushXHTML(p.tmpDir); err != nil {
		return "", err
	}
	if err := p.Book.flushMeta(p.tmpDir); err != nil {
		return "", err
	}

	fname := p.prepareOutputName()

	var err error
	switch p.format {
	case OEpub:
		err = p.FinalizeEPUB(fname)
	case OMobi:
		err = p.FinalizeMOBI(fname)
	case OAzw3:
		err = p.FinalizeAZW3(fname)
	}
	return fname, err
}

// SendToKindle will mail converted file to specified address and remove file if requested.
func (p *Processor) SendToKindle(fname string) error {

	if !p.stk || p.format != OMobi || len(fname) == 0 {
		return nil
	}

	if !p.env.Cfg.SMTPConfig.IsValid() {
		p.env.Log.Warn("Configuration for Send To Kindle is incorrect, skipping", zap.Any("configuration", p.env.Cfg.SMTPConfig))
		return nil
	}

	start := time.Now()
	p.env.Log.Debug("Sending content to Kindle - starting",
		zap.String("from", p.env.Cfg.SMTPConfig.From),
		zap.String("to", p.env.Cfg.SMTPConfig.To),
		zap.String("file", fname),
	)
	defer func(start time.Time) {
		p.env.Log.Debug("Sending content to Kindle - done", zap.Duration("elapsed", time.Now().Sub(start)))
	}(start)

	m := gomail.NewMessage()
	m.SetHeader("From", p.env.Cfg.SMTPConfig.From)
	m.SetAddressHeader("To", p.env.Cfg.SMTPConfig.To, "kindle")
	m.SetHeader("Subject", "Sent to Kindle")
	m.SetBody("text/plain", "This email has been automatically sent by fb2converter tool")
	m.Attach(fname)

	d := gomail.NewDialer(p.env.Cfg.SMTPConfig.Server, p.env.Cfg.SMTPConfig.Port, p.env.Cfg.SMTPConfig.User, p.env.Cfg.SMTPConfig.Password)

	if err := d.DialAndSend(m); err != nil {
		return errors.Wrap(err, "SentToKindle failed")
	}

	if p.env.Cfg.SMTPConfig.DeleteOnSuccess {
		p.env.Log.Debug("Deleting after send", zap.String("location", fname))
		if err := os.Remove(fname); err != nil {
			p.env.Log.Warn("Unable to delete after send", zap.String("location", fname), zap.Error(err))
		}
		if !p.nodirs {
			// remove all empty directories in the path following p.dst
			for outDir := filepath.Dir(fname); outDir != p.dst; outDir = filepath.Dir(outDir) {
				if err := os.Remove(outDir); err != nil {
					p.env.Log.Warn("Unable to delete after send", zap.String("location", outDir), zap.Error(err))
				}
			}
		}
	}
	return nil
}

// Clean removes temporary files left after processing.
func (p *Processor) Clean() error {
	if p.env.Debug {
		// Leave temporary files intact
		return nil
	}
	p.env.Log.Debug("Cleaning", zap.String("location", p.tmpDir))
	return os.RemoveAll(p.tmpDir)
}

// prepareOutputName generates output file name.
func (p *Processor) prepareOutputName() string {

	var outDir string
	if !p.nodirs {
		outDir = filepath.Dir(p.src)
	}
	outDir = filepath.Join(p.dst, outDir)

	outFile := strings.TrimSuffix(filepath.Base(p.src), filepath.Ext(p.src)) + "." + p.format.String()
	if len(p.env.Cfg.Doc.FileNameFormat) > 0 {
		name := config.CleanFileName(
			ReplaceKeywords(p.env.Cfg.Doc.FileNameFormat, CreateFileNameKeywordsMap(p.Book, p.env.Cfg.Doc.SeqNumPos)),
		)
		if len(name) > 0 {
			outFile = name + "." + p.format.String()
		}
	}
	return filepath.Join(outDir, outFile)
}

// processDescription processes book description element.
func (p *Processor) processDescription() error {

	start := time.Now()
	p.env.Log.Debug("Parsing description - start")
	defer func(start time.Time) {
		p.env.Log.Debug("Parsing description - done",
			zap.Duration("elapsed", time.Now().Sub(start)),
			zap.Stringer("id", p.Book.ID),
			zap.String("title", p.Book.Title),
			zap.Stringer("lang", p.Book.Lang),
			zap.String("cover", p.Book.Cover),
			zap.Strings("genres", p.Book.Genres),
			zap.Strings("authors", p.Book.Authors),
			zap.String("sequence", p.Book.SeqName),
			zap.Int("sequence number", p.Book.SeqNum),
			zap.String("date", p.Book.Date),
		)
	}(start)

	for _, desc := range p.doc.FindElements("./FictionBook/description") {

		if info := desc.SelectElement("document-info"); info != nil {
			if id := info.SelectElement("id"); id != nil {
				text := strings.TrimSpace(id.Text())
				if u, err := uuid.Parse(text); err == nil {
					p.Book.ID = u
				} else {
					p.env.Log.Debug("Unable to parse book id, deriving new", zap.String("id", text), zap.Error(err))
					p.Book.ID = uuid.NewSHA1(nameSpaceFB2, []byte(text))
				}
			}
		}
		if info := desc.SelectElement("title-info"); info != nil {
			if e := info.SelectElement("book-title"); e != nil {
				if t := strings.TrimSpace(e.Text()); len(t) > 0 {
					p.Book.Title = t
				}
			}
			if e := info.SelectElement("lang"); e != nil {
				if l := strings.TrimSpace(e.Text()); len(l) > 0 {
					t, err := language.Parse(l)
					if err != nil {
						// last resort - try names directly
						for _, st := range display.Supported.Tags() {
							if strings.EqualFold(display.Self.Name(st), l) {
								t = st
								err = nil
								break
							}
						}
						if err != nil {
							return err
						}
					}
					p.Book.Lang = t
					if p.env.Cfg.Doc.Hyphenate {
						p.Book.hyph = newHyph(t, p.env.Log)
					}
				}
			}
			if e := info.SelectElement("coverpage"); e != nil {
				if i := e.SelectElement("image"); i != nil {
					c := getAttrValue(i, "href")
					if len(c) > 0 {
						if u, err := url.Parse(c); err != nil {
							p.env.Log.Warn("Unable to parse cover image href", zap.String("href", c), zap.Error(err))
						} else {
							p.Book.Cover = u.Fragment
						}
					}
				}
			}
			for _, e := range info.SelectElements("genre") {
				if g := strings.TrimSpace(e.Text()); len(g) > 0 {
					p.Book.Genres = append(p.Book.Genres, g)
				}
			}
			for _, e := range info.SelectElements("author") {
				p.Book.Authors = append(p.Book.Authors, ReplaceKeywords(p.env.Cfg.Doc.AuthorFormat, CreateAuthorKeywordsMap(e)))
			}
			if e := info.SelectElement("sequence"); e != nil {
				var err error
				p.Book.SeqName = getAttrValue(e, "name")
				num := getAttrValue(e, "number")
				if len(num) > 0 {
					if !govalidator.IsInt(num) {
						p.env.Log.Warn("Sequence number is not an integer, ignoring", zap.String("xml", getXMLFragmentFromElement(e)))
					} else {
						p.Book.SeqNum, err = strconv.Atoi(num)
						if err != nil {
							p.env.Log.Warn("Unable to parse sequence number, ignoring", zap.String("number", getAttrValue(e, "number")), zap.Error(err))
						}
					}
				}
			}
			if e := info.SelectElement("annotation"); e != nil {
				p.Book.Annotation = getTextFragment(e)
				if p.env.Cfg.Doc.Annotation.Create {
					to, f := p.ctx().createXHTML("annotation")
					inner := to.AddNext("div", attr("class", "annotation"))
					inner.AddNext("div", attr("class", "h1")).SetText(p.env.Cfg.Doc.Annotation.Title)
					if err := p.transfer(e, inner, "div"); err != nil {
						p.env.Log.Warn("Unable to parse annotation", zap.String("path", e.GetPath()), zap.Error(err))
					} else {
						p.Book.Files = append(p.Book.Files, f)
					}
				}
			}
			if e := info.SelectElement("date"); e != nil {
				p.Book.Date = getTextFragment(e)
			}
		}
	}
	return nil
}

// processBodies processes book bodies, including main one.
func (p *Processor) processBodies() error {

	start := time.Now()
	p.env.Log.Debug("Parsing bodies - start")
	defer func(start time.Time) {
		p.env.Log.Debug("Parsing bodies - done",
			zap.Duration("elapsed", time.Now().Sub(start)),
		)
	}(start)

	for i, body := range p.doc.FindElements("./FictionBook/body") {
		if err := p.processBody(i, body); err != nil {
			return err
		}
	}

	return nil
}

// processNotes processes notes bodies. We will need notes when main body is parsed.
func (p *Processor) processNotes() error {

	start := time.Now()
	p.env.Log.Debug("Parsing notes - start")
	defer func(start time.Time) {
		p.env.Log.Debug("Parsing notes - done",
			zap.Duration("elapsed", time.Now().Sub(start)),
			zap.Int("body titles", len(p.Book.NoteBodyTitles)),
		)
	}(start)

	for _, el := range p.doc.FindElements("./FictionBook/body[@name]") {

		name := getAttrValue(el, "name")
		if !IsOneOf(name, p.env.Cfg.Doc.Notes.BodyNames) {
			continue
		}

		for _, section := range el.ChildElements() {

			// Sometimes note section has separate title - we want to use it in TOC
			if section.Tag == "title" {
				t := SanitizeTitle(getTextFragment(section))
				if len(t) > 0 {
					ctx := p.ctxPush()
					ctx.inHeader = true
					if err := p.transfer(section, &ctx.out.Element, "div", "h0"); err != nil {
						p.env.Log.Warn("Unable to parse notes body title", zap.String("path", section.GetPath()), zap.Error(err))
					}
					ctx.inHeader = false
					p.ctxPop()

					child := ctx.out.FindElement("./*")
					p.Book.NoteBodyTitles[name] = &note{
						title:  t,
						body:   getTextFragment(child),
						parsed: child.Copy(),
					}
				}
				continue
			}

			if section.Tag == "section" && getAttrValue(section, "id") != "" {
				id := getAttrValue(section, "id")
				note := &note{}
				for _, c := range section.ChildElements() {
					t := getTextFragment(c)
					if c.Tag == "title" {
						note.title = SanitizeTitle(t)
					} else {
						note.body += t
					}
				}
				p.Book.NotesOrder = append(p.Book.NotesOrder, notelink{id: id, bodyName: name})
				p.Book.Notes[id] = note
				continue
			}
		}
	}
	return nil
}

// processBinaries processes book images.
func (p *Processor) processBinaries() error {

	start := time.Now()
	p.env.Log.Debug("Parsing images - start")
	defer func(start time.Time) {
		p.env.Log.Debug("Parsing images - done",
			zap.Duration("elapsed", time.Now().Sub(start)),
			zap.Int("images", len(p.Book.Images)),
		)
	}(start)

	for i, el := range p.doc.FindElements("./FictionBook/binary[@id]") {

		id := getAttrValue(el, "id")
		declaredCT := getAttrValue(el, "content-type")

		s := strings.TrimSpace(el.Text())
		// some files are badly formatted
		s = strings.Replace(s, " ", "", -1)
		data, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return errors.Wrapf(err, "unable to decode binary (%s)", id)
		}

		if strings.HasSuffix(strings.ToLower(declaredCT), "svg") {
			// Special case - do not touch SVG
			p.Book.Images = append(p.Book.Images, &binary{
				log:     p.env.Log,
				id:      id,
				ct:      "image/svg+xml",
				fname:   fmt.Sprintf("bin%08d.svg", i),
				relpath: filepath.Join(DirContent, DirImages),
				imgType: "svg",
				data:    data,
			})
			continue
		}

		var (
			detectedCT string
			doNotTouch bool
		)

		img, imgType, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			p.env.Log.Warn("Unable to decode image",
				zap.String("id", id),
				zap.String("declared", declaredCT),
				zap.Error(err))

			if !p.env.Cfg.Doc.UseBrokenImages {
				continue
			}

			detectedCT = declaredCT
			doNotTouch = true
		} else {
			detectedCT = mime.TypeByExtension("." + imgType)
		}

		if !strings.EqualFold(declaredCT, detectedCT) {
			p.env.Log.Warn("Declared and detected image types do not match, using detected type",
				zap.String("id", id),
				zap.String("declared", declaredCT),
				zap.String("detected", detectedCT))
		}

		// fill in image info
		b := &binary{
			log:     p.env.Log,
			id:      id,
			ct:      detectedCT,
			fname:   fmt.Sprintf("bin%08d.%s", i, imgType),
			relpath: filepath.Join(DirContent, DirImages),
			img:     img,
			imgType: imgType,
			data:    data,
		}

		if !doNotTouch {
			// see if any additional processing is requested
			if !isImageSupported(b.imgType) && (p.format == OMobi || p.format == OAzw3) {
				b.flags |= imageKindle
			}
			if p.env.Cfg.Doc.RemovePNGTransparency && imgType == "png" {
				b.flags |= imageOpaquePNG
			}
			if p.env.Cfg.Doc.ImagesScaleFactor > 0 && (imgType == "png" || imgType == "jpeg") {
				b.flags |= imageScale
				b.scaleFactor = p.env.Cfg.Doc.ImagesScaleFactor
			}
		}
		p.Book.Images = append(p.Book.Images, b)
	}
	return nil
}

// processLinks goes over generated documents and makes sure hanging anchors are properly anchored.
func (p *Processor) processLinks() error {

	start := time.Now()
	p.env.Log.Debug("Processing links - start")
	defer func(start time.Time) {
		p.env.Log.Debug("Processing links - done",
			zap.Duration("elapsed", time.Now().Sub(start)),
		)
	}(start)

	for _, f := range p.Book.Files {
		if f.doc == nil {
			continue
		}
		for _, a := range f.doc.FindElements("//a[@href]") {
			href := getAttrValue(a, "href")
			if !strings.HasPrefix(href, "#") {
				continue
			}
			if fname, ok := p.Book.LinksLocations[href[1:]]; ok {
				a.CreateAttr("href", fname+href)
			}
		}
	}
	return nil
}

// processImages makes sure that images we use have suitable properties.
func (p *Processor) processImages() error {

	start := time.Now()
	p.env.Log.Debug("Processing images - start")
	defer func(start time.Time) {
		p.env.Log.Debug("Processing images - done",
			zap.Duration("elapsed", time.Now().Sub(start)),
		)
	}(start)

	if len(p.Book.Cover) > 0 {
		// some badly formatted fb2 have several covers (LibRusEq - engineers with two left feet)
		// leave only first one
		haveFirstCover, haveExtraCovers := false, false
		for i, b := range p.Book.Images {
			if b.id == p.Book.Cover {
				if haveFirstCover {
					haveExtraCovers = true
					p.Book.Images[i].id = "" // mark for removal
				} else {
					haveFirstCover = true
					// NOTE: We will process cover separately
					b.flags &= ^imageScale
					b.scaleFactor = 0
				}
			}
		}
		if haveExtraCovers {
			p.env.Log.Warn("Removing cover image duplicates, leaving only the first one")
			for i := len(p.Book.Images) - 1; i >= 0; i-- {
				if p.Book.Images[i].id == "" {
					p.Book.Images = append(p.Book.Images[:i], p.Book.Images[i+1:]...)
				}
			}
		}
	} else if p.env.Cfg.Doc.Cover.Default || p.format == OMobi || p.format == OAzw3 {
		// For Kindle we always supply cover image if none is present, for others - only if asked to
		b, err := p.getDefaultCover(len(p.Book.Images))
		if err != nil {
			// not found or cannot be decoded, misconfiguration - stop here
			return err
		}
		p.env.Log.Debug("Providing default cover image")
		p.Book.Cover = b.id
		p.Book.Images = append(p.Book.Images, b)
		if p.stampPlacement == StampNone {
			// default cover always stamped
			p.stampPlacement = StampMiddle
		}
	}
	return nil
}

// shortcuts
func (p *Processor) ctx() *context {
	return p.Book.ctx()
}

func (p *Processor) ctxPush() *context {
	return p.Book.ctxPush()
}

func (p *Processor) ctxPop() *context {
	return p.Book.ctxPop()
}