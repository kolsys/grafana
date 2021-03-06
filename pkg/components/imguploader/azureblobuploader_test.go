package imguploader

import (
	"context"
	"testing"

	"github.com/p0hil/grafana/pkg/setting"
	. "github.com/smartystreets/goconvey/convey"
)

func TestUploadToAzureBlob(t *testing.T) {
	SkipConvey("[Integration test] for external_image_store.azure_blob", t, func() {
		err := setting.NewConfigContext(&setting.CommandLineArgs{
			HomePath: "../../../",
		})

		uploader, _ := NewImageUploader()

		path, err := uploader.Upload(context.Background(), "../../../public/img/logo_transparent_400x.png")

		So(err, ShouldBeNil)
		So(path, ShouldNotEqual, "")
	})
}
