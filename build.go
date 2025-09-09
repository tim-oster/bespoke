package main

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/urfave/cli/v3"
)

var buildCmd = &cli.Command{
	Name:   "build",
	Usage:  "build a binary into a docker image",
	Action: buildAction,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "tag",
			Aliases: []string{"t"},
			Usage:   "tag to use for the output image",
			Value:   "bespoke:latest",
		},
		&cli.StringFlag{
			Name:    "out",
			Aliases: []string{"o"},
			Usage:   "path to the output image",
			Value:   "out.tar",
		},
		&cli.StringFlag{
			Name: "push",
			Usage: `
				Push the image to a registry (example: europe-west1-docker.pkg.dev/your-project/your-registry/image:latest).
				Requires docker credentials to be set up, e.g. via "$HOME/.docker/config.json" or "$DOCKER_CONFIG/config.json".
				See https://pkg.go.dev/github.com/google/go-containerregistry/pkg/authn for more information.
			`,
		},
	},
}

func buildAction(ctx context.Context, c *cli.Command) error {
	if c.IsSet("push") && c.IsSet("out") {
		return fmt.Errorf("push and out flags cannot be used together")
	}

	cfg, err := loadConfig(c)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if len(cfg.Services) == 0 {
		return fmt.Errorf("no services found in config")
	}

	// build binaries
	var (
		svcNames []string
		binaries []string
	)
	for _, service := range cfg.Services {
		service.ConfigDefaults = cfg.Defaults.merge(service.ConfigDefaults)
		binary, err := buildBinary(ctx, service, cfg.ProjectRoot)
		if err != nil {
			return fmt.Errorf("failed to build binary: %w", err)
		}
		svcNames = append(svcNames, service.Name)
		binaries = append(binaries, binary)
	}

	// build image
	image := empty.Image
	image, err = addBinariesLayer(image, svcNames, binaries)
	if err != nil {
		return fmt.Errorf("failed to add binaries layer: %w", err)
	}
	if !cfg.WithoutCABundle {
		image, err = addCACertsLayer(image)
		if err != nil {
			return fmt.Errorf("failed to add CA certs layer: %w", err)
		}
	}

	if c.String("push") != "" {
		// push image to registry

		slog.Info("pushing image to registry", "ref", c.String("push"))

		ref, err := name.ParseReference(c.String("push"))
		if err != nil {
			return fmt.Errorf("failed to parse reference: %w", err)
		}
		if err := remote.Write(ref, image, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain)); err != nil {
			return fmt.Errorf("failed to push image to registry: %w", err)
		}
	} else {
		// write image to file

		tag, err := name.NewTag(c.String("tag"))
		if err != nil {
			return fmt.Errorf("failed to create tag: %w", err)
		}
		if err := tarball.WriteToFile(c.String("out"), tag, image); err != nil {
			return fmt.Errorf("failed to write image to file: %w", err)
		}
	}

	return nil
}

func buildBinary(ctx context.Context, svc ConfigService, projectRoot string) (file string, err error) {
	const (
		timetzdataTag = "timetzdata"
	)

	// create temp file and delete on error
	f, err := os.CreateTemp("", fmt.Sprintf("bespoke-binary-%s-*", svc.Name))
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()

	// compile all go tags
	var tags []string
	if svc.Tags != nil {
		tags = *svc.Tags
	}
	if !svc.WithoutTimeTZData && !slices.Contains(tags, timetzdataTag) {
		tags = append(tags, timetzdataTag)
	}

	// gather final list of arguments
	var args = []string{
		"build",
		"-o", f.Name(),
	}
	if len(tags) != 0 {
		args = append(args, "-tags", strings.Join(tags, ","))
	}
	if svc.ConfigDefaults.AdditionalFlags != nil {
		args = append(args, *svc.ConfigDefaults.AdditionalFlags...)
	}
	args = append(args, svc.Package) // has to be last

	// determine which go binary to use
	goBin := "go"
	if alt := os.Getenv("BESPOKE_GO_BIN"); alt != "" {
		goBin = alt
	}

	// construct environment variables
	env := os.Environ()
	if svc.GOOS != "" {
		env = append(env, "GOOS="+svc.GOOS)
	}
	if svc.GOARCH != "" {
		env = append(env, "GOARCH="+svc.GOARCH)
	}

	// build the binary
	cmd := exec.CommandContext(ctx, goBin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Dir = projectRoot
	cmd.Env = env

	slog.Info("building binary", "service", svc.Name, "cmd", cmd.String(), "GOOS", svc.GOOS, "GOARCH", svc.GOARCH)

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to build binary: %w", err)
	}

	return f.Name(), nil
}

func addBinariesLayer(image v1.Image, svcNames, binaryPaths []string) (v1.Image, error) {
	var (
		layerPaths []string
		layerData  [][]byte
	)

	if len(svcNames) != len(binaryPaths) {
		panic("svcNames and binaryPaths must have the same length")
	}
	if len(svcNames) == 0 {
		return nil, fmt.Errorf("no services found")
	}

	for i, binaryPath := range binaryPaths {
		binaryData, err := os.ReadFile(binaryPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read binary file: %w", err)
		}
		layerPaths = append(layerPaths, "/bin/"+svcNames[i])
		layerData = append(layerData, binaryData)
	}

	binaryLayer, err := createTarLayer(layerPaths, layerData)
	if err != nil {
		return nil, fmt.Errorf("failed to create binary tar layer: %w", err)
	}

	image, err = mutate.AppendLayers(image, binaryLayer)
	if err != nil {
		return nil, fmt.Errorf("failed to append layers: %w", err)
	}

	image, err = mutate.Config(image, v1.Config{
		Entrypoint: []string{layerPaths[0]}, // default to first service
	})
	if err != nil {
		return nil, fmt.Errorf("failed to set entrypoint: %w", err)
	}

	return image, nil
}

func addCACertsLayer(image v1.Image) (v1.Image, error) {
	caCerts, err := downloadCACerts()
	if err != nil {
		return nil, fmt.Errorf("failed to download CA certificates: %w", err)
	}

	caLayer, err := createTarLayer([]string{"/etc/ssl/certs/ca-certificates.crt"}, [][]byte{caCerts})
	if err != nil {
		return nil, fmt.Errorf("failed to create CA certs tar layer: %w", err)
	}

	image, err = mutate.AppendLayers(image, caLayer)
	if err != nil {
		return nil, fmt.Errorf("failed to append layers: %w", err)
	}

	return image, nil
}

func downloadCACerts() ([]byte, error) {
	resp, err := http.Get("https://curl.se/ca/cacert.pem")
	if err != nil {
		return nil, fmt.Errorf("failed to download CA certificates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download CA certificates: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA certificates response: %w", err)
	}

	return data, nil
}

func createTarLayer(filePaths []string, data [][]byte) (v1.Layer, error) {
	if len(filePaths) != len(data) {
		return nil, fmt.Errorf("filePaths and data must have the same length")
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	for i, filePath := range filePaths {
		fileData := data[i]
		header := &tar.Header{
			Name: filePath,
			Mode: 0755,
			Size: int64(len(fileData)),
		}

		if err := tw.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("failed to write tar header for %s: %w", filePath, err)
		}

		if _, err := tw.Write(fileData); err != nil {
			return nil, fmt.Errorf("failed to write tar data for %s: %w", filePath, err)
		}
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close tar writer: %w", err)
	}

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create layer: %w", err)
	}

	return layer, nil
}
