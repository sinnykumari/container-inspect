package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/containers/image/image"
	"github.com/containers/image/manifest"
	"github.com/containers/image/transports/alltransports"
	"github.com/containers/image/types"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

const (
	// the number of times to retry commands that pull data from the network
	numRetriesNetCommands = 5
	// Pull secret.  Written by the machine-config-operator
	kubeletAuthFile = "/var/lib/kubelet/config.json"
)

// inspectOutput is the output format of (skopeo inspect), primarily so that we can format it with a simple json.MarshalIndent.
type inspectOutput struct {
	Name          string `json:",omitempty"`
	Tag           string `json:",omitempty"`
	Digest        digest.Digest
	RepoTags      []string
	Created       *time.Time
	DockerVersion string
	Labels        map[string]string
	Architecture  string
	Os            string
	Layers        []string
	Env           []string
}

func retryIfNecessary(ctx context.Context, operation func() error) error {
	err := operation()
	for attempt := 0; err != nil && attempt < numRetriesNetCommands; attempt++ {
		delay := time.Duration(int(math.Pow(2, float64(attempt)))) * time.Second
		fmt.Printf("Warning: failed, retrying in %s ... (%d/%d)", delay, attempt+1, numRetriesNetCommands)
		select {
		case <-time.After(delay):
			break
		case <-ctx.Done():
			return err
		}
		err = operation()
	}
	return err
}

// parseImageSource converts image URL-like string to an ImageSource.
// The caller must call .Close() on the returned ImageSource.
func parseImageSource(ctx context.Context, name string) (types.ImageSource, error) {
	ref, err := alltransports.ParseImageName(name)
	if err != nil {
		return nil, err
	}
	sys := &types.SystemContext{}
	if err != nil {
		return nil, err
	}
	return ref.NewImageSource(ctx, sys)
}

// This function has been inspired from upstream skopeo inspect, see https://github.com/containers/skopeo/blob/master/cmd/skopeo/inspect.go
// We can use skopeo inspect directly once fetching RepoTags becomes optional in skopeo.
func inspect(imageName string) (retErr error) {
	var (
		rawManifest []byte
		src         types.ImageSource
		imgInspect  *types.ImageInspectInfo
		err         error
	)

	ctx := context.Background()
	sys := &types.SystemContext{}
	sys.AuthFilePath = kubeletAuthFile

	if src, err = parseImageSource(ctx, imageName); err != nil {
		return err
	}

	if err := retryIfNecessary(ctx, func() error {
		src, err = parseImageSource(ctx, imageName)
		return err
	}); err != nil {
		return errors.Wrapf(err, "Error parsing image name %q", imageName)
	}

	defer func() {
		if err = src.Close(); err != nil {
			retErr = errors.Wrapf(retErr, fmt.Sprintf("(could not close image: %v) ", err))
		}
	}()

	if err := retryIfNecessary(ctx, func() error {
		rawManifest, _, err = src.GetManifest(ctx, nil)
		return err
	}); err != nil {
		return errors.Wrapf(err, "Error retrieving manifest for image")
	}

	img, err := image.FromUnparsedImage(ctx, sys, image.UnparsedInstance(src, nil))
	if err != nil {
		return fmt.Errorf("Error parsing manifest for image: %v", err)
	}

	if err := retryIfNecessary(ctx, func() error {
		imgInspect, err = img.Inspect(ctx)
		return err
	}); err != nil {
		return err
	}

	outputData := inspectOutput{
		Name: "", // Set below if DockerReference() is known
		Tag:  imgInspect.Tag,
		// To speed up the inspect, we are not fetching tags for our usecase
		RepoTags:      []string{},
		Created:       imgInspect.Created,
		DockerVersion: imgInspect.DockerVersion,
		Labels:        imgInspect.Labels,
		Architecture:  imgInspect.Architecture,
		Os:            imgInspect.Os,
		Layers:        imgInspect.Layers,
		Env:           imgInspect.Env,
	}
	outputData.Digest, err = manifest.Digest(rawManifest)
	if err != nil {
		return fmt.Errorf("Error computing manifest digest: %v", err)
	}

	if dockerRef := img.Reference().DockerReference(); dockerRef != nil {
		outputData.Name = dockerRef.Name()
	}

	fmt.Printf("Commit : %s  Version: %s \n", outputData.Labels["com.coreos.ostree-commit"], outputData.Labels["version"])

	return nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Errorf("Specify container image name")
	}
	containerImage := os.Args[1]
	if err := inspect("docker://" + containerImage); err != nil {
		fmt.Errorf("Failed inspecting image", err)
	}
}
