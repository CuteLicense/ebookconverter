package processor

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"io/ioutil"
	"os"
	"path/filepath"

	// additional supported image formats
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/disintegration/imaging"
	"github.com/pkg/errors"
	"go.uber.org/zap"

	"fb2converter/processor/internal/mobi"
)

type binaryProcessingFlags uint8

const (
	imageKindle binaryProcessingFlags = 1 << iota
	imageOpaquePNG
	imageScale
)

type binary struct {
	log *zap.Logger
	//
	id          string
	ct          string
	fname       string
	relpath     string // always relative to "root" directory - usually temporary working directory
	flags       binaryProcessingFlags
	scaleFactor float64
	img         image.Image
	imgType     string
	data        []byte
}

// flush is storing image to file
func (b *binary) flush(path string) error {

	// Sanity
	if len(b.fname) == 0 || (len(b.data) == 0 && b.img == nil) {
		return nil
	}

	newdir := filepath.Join(path, b.relpath)
	if err := os.MkdirAll(newdir, 0700); err != nil {
		return errors.Wrapf(err, "unable to create directory %s", newdir)
	}

	// Do not touch svg images
	if b.imgType == "svg" {
		goto Storing
	}

	// See if processing is needed
	if b.flags != 0 {

		// Just in case
		if b.img == nil && len(b.data) != 0 {
			// image was not decoded yet
			var err error
			b.img, b.imgType, err = image.Decode(bytes.NewReader(b.data))
			if err != nil {
				b.log.Warn("Unable to decode image for processing, storing as is",
					zap.String("id", b.id),
					zap.Error(err))
				goto Storing
			}
		}

		// Scaling
		if b.flags&imageScale != 0 {
			if resizedImg := imaging.Resize(b.img,
				int(float64(b.img.Bounds().Dx())*b.scaleFactor),
				int(float64(b.img.Bounds().Dy())*b.scaleFactor),
				imaging.Linear); resizedImg != nil {
				b.img = resizedImg
			} else {
				b.log.Warn("Unable to resize image, storing as is",
					zap.String("id", b.id))
				goto Storing
			}
		}

		// PNG transparency
		if b.flags&imageOpaquePNG != 0 {

			opaque := func(im image.Image) bool {
				if oimg, ok := im.(interface{ Opaque() bool }); ok {
					return oimg.Opaque()
				}
				return true
			}(b.img)

			if !opaque {
				b.log.Debug("Removing PNG transparency", zap.String("id", b.id))
				opaqueImg := image.NewRGBA(b.img.Bounds())
				draw.Draw(opaqueImg, b.img.Bounds(), &image.Uniform{color.RGBA{255, 255, 255, 255}}, image.ZP, draw.Src)
				draw.Draw(opaqueImg, b.img.Bounds(), b.img, image.ZP, draw.Over)
				b.img = opaqueImg
			}
		}

		targetType := b.imgType

		// Unsupported format
		if b.flags&imageKindle != 0 {
			if targetType != "jpeg" {
				b.log.Warn("Image type is not supported by targeted device, converting to jpeg",
					zap.String("id", b.id),
					zap.String("type", b.imgType))
				targetType = "jpeg"
			}
		}

		// Serialize the results
		var buf = new(bytes.Buffer)
		if targetType == "png" {
			if err := imaging.Encode(buf, b.img, imaging.PNG); err != nil {
				b.log.Error("Unable to encode processed PNG, skipping",
					zap.String("id", b.id),
					zap.Error(err))
				goto Storing
			}
			b.imgType = "png"
			b.ct = "image/png"
		} else if targetType == "jpeg" {

			if err := imaging.Encode(buf, b.img, imaging.JPEG, imaging.JPEGQuality(75)); err != nil {
				b.log.Error("Unable to encode processed image, skipping",
					zap.String("id", b.id),
					zap.Error(err))
				goto Storing
			}
			b.imgType = "jpeg"
			b.ct = "image/jpeg"

			var jfifAdded bool
			buf, jfifAdded = mobi.SetJpegDPI(buf, mobi.DpiPxPerInch, 300, 300)
			if jfifAdded {
				b.log.Debug("Inserting jpeg JFIF APP0 marker segment", zap.String("id", b.id))
			}

		} else {
			b.log.Warn("Unable to process image - unsupported format, skipping",
				zap.String("id", b.id),
				zap.String("type", b.imgType))
			goto Storing
		}
		b.data = buf.Bytes()
	}

	// Sanity - should never happen
	if len(b.data) == 0 {
		return errors.Errorf("No image to save %s (%s)", b.id, filepath.Join(newdir, b.fname))
	}

Storing:
	if err := ioutil.WriteFile(filepath.Join(newdir, b.fname), b.data, 0644); err != nil {
		return errors.Wrapf(err, "unable to save image (%s)", filepath.Join(newdir, b.fname))
	}
	return nil
}