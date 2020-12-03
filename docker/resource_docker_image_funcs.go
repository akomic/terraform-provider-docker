package docker

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"

	"bytes"
	"encoding/base64"
	"encoding/json"

	"github.com/docker/cli/cli/command/image/build"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/hashicorp/terraform-plugin-sdk/helper/schema"
	homedir "github.com/mitchellh/go-homedir"
)

var (
	pullOutput string
	pushOutput string
)

func getBuildContext(filePath string, excludes []string) io.Reader {
	filePath, _ = homedir.Expand(filePath)
	ctx, _ := archive.TarWithOptions(filePath, &archive.TarOptions{
		ExcludePatterns: excludes,
	})
	return ctx
}

func decodeBuildMessages(response types.ImageBuildResponse) (string, error) {
	buf := new(bytes.Buffer)
	buildErr := error(nil)

	dec := json.NewDecoder(response.Body)
	for dec.More() {
		var m jsonmessage.JSONMessage
		err := dec.Decode(&m)
		if err != nil {
			return buf.String(), fmt.Errorf("Problem decoding message from docker daemon: %s", err)
		}

		m.Display(buf, false)

		if m.Error != nil {
			buildErr = fmt.Errorf("Unable to build image")
		}
	}
	log.Printf("[DEBUG] build: %s", buf.String())

	return buf.String(), buildErr
}

func decodePushPullMessages(responseBody io.Reader) (string, error) {
	buf := new(bytes.Buffer)
	buildErr := error(nil)

	dec := json.NewDecoder(responseBody)
	for dec.More() {
		var m jsonmessage.JSONMessage
		err := dec.Decode(&m)
		if err != nil {
			return buf.String(), fmt.Errorf("Problem decoding message from docker daemon: %s", err)
		}

		m.Display(buf, false)

		if m.Error != nil {
			buildErr = fmt.Errorf("Unable to build image")
		}
	}
	log.Printf("[DEBUG] push-pull: %s", buf.String())

	return buf.String(), buildErr
}

func resourceDockerImageCreate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*ProviderConfig).DockerClient
	imageName := d.Get("name").(string)

	if value, ok := d.GetOk("build"); ok {
		doBuild := d.Get("force_build").(bool)

		if !doBuild {
			_, err := findImage(imageName, client, meta.(*ProviderConfig).AuthConfigs)
			if err != nil {
				doBuild = true
				log.Printf("[DEBUG] Error pulling image [%s]: %v", imageName, err)
			}
		}
		if doBuild {
			for _, rawBuild := range value.(*schema.Set).List() {
				rawBuild := rawBuild.(map[string]interface{})

				buildOutput, err := buildDockerImage(rawBuild, imageName, client)

				d.Set("build_output", buildOutput)

				if err != nil {
					return fmt.Errorf("%s\n\n%s", err, buildOutput)
				}
			}
		}
	}
	apiImage, err := findImage(imageName, client, meta.(*ProviderConfig).AuthConfigs)
	if err != nil {
		return fmt.Errorf("Unable to read Docker image into resource: %s", err)
	}

	d.SetId(apiImage.ID + d.Get("name").(string))

	if pushRemote := d.Get("push_remote").(bool); pushRemote {
		if err := pushImage(client, meta.(*ProviderConfig).AuthConfigs, imageName); err != nil {
			return fmt.Errorf("Unable to push image [%s]: %s", imageName, err)
		}
	}
	return resourceDockerImageRead(d, meta)
}

func resourceDockerImageRead(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*ProviderConfig).DockerClient
	var data Data
	if err := fetchLocalImages(&data, client); err != nil {
		return fmt.Errorf("Error reading docker image list: %s", err)
	}
	for id := range data.DockerImages {
		log.Printf("[DEBUG] local images data: %v", id)
	}
	foundImage := searchLocalImages(data, d.Get("name").(string))

	if foundImage == nil {
		d.SetId("")
		return nil
	}

	d.SetId(foundImage.ID + d.Get("name").(string))
	d.Set("latest", foundImage.ID)

	if pullOutput != "" {
		d.Set("pull_output", pullOutput)
	}
	if pushOutput != "" {
		d.Set("push_output", pushOutput)
	}

	return nil
}

func resourceDockerImageUpdate(d *schema.ResourceData, meta interface{}) error {
	// We need to re-read in case switching parameters affects
	// the value of "latest" or others
	client := meta.(*ProviderConfig).DockerClient
	imageName := d.Get("name").(string)
	apiImage, err := findImage(imageName, client, meta.(*ProviderConfig).AuthConfigs)
	if err != nil {
		return fmt.Errorf("Unable to read Docker image into resource: %s", err)
	}

	d.Set("latest", apiImage.ID)
	if pushRemote := d.Get("push_remote").(bool); pushRemote {
		if err := pushImage(client, meta.(*ProviderConfig).AuthConfigs, imageName); err != nil {
			return fmt.Errorf("Unable to push image [%s]: %s", imageName, err)
		}
	}

	return resourceDockerImageRead(d, meta)
}

func resourceDockerImageDelete(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*ProviderConfig).DockerClient
	err := removeImage(d, client)
	if err != nil {
		return fmt.Errorf("Unable to remove Docker image: %s", err)
	}
	d.SetId("")
	return nil
}

func searchLocalImages(data Data, imageName string) *types.ImageSummary {
	log.Print("[DEBUG] searching local images")

	if apiImage, ok := data.DockerImages[imageName]; ok {
		log.Printf("[DEBUG] found local image via imageName: %v", imageName)
		return apiImage
	}
	if apiImage, ok := data.DockerImages[imageName+":latest"]; ok {
		log.Printf("[DEBUG] found local image via imageName + latest: %v", imageName)
		imageName = imageName + ":latest"
		return apiImage
	}
	return nil
}

func removeImage(d *schema.ResourceData, client *client.Client) error {
	var data Data

	if keepLocally := d.Get("keep_locally").(bool); keepLocally {
		return nil
	}

	if err := fetchLocalImages(&data, client); err != nil {
		return err
	}

	imageName := d.Get("name").(string)
	if imageName == "" {
		return fmt.Errorf("Empty image name is not allowed")
	}

	foundImage := searchLocalImages(data, imageName)

	if foundImage != nil {
		imageDeleteResponseItems, err := client.ImageRemove(context.Background(), foundImage.ID, types.ImageRemoveOptions{})
		if err != nil {
			return err
		}
		log.Printf("[INFO] Deleted image items: %v", imageDeleteResponseItems)
	}

	return nil
}

func fetchLocalImages(data *Data, client *client.Client) error {
	log.Print("[DEBUG] fetching local images")
	images, err := client.ImageList(context.Background(), types.ImageListOptions{All: false})
	if err != nil {
		return fmt.Errorf("Unable to list Docker images: %s", err)
	}

	if data.DockerImages == nil {
		data.DockerImages = make(map[string]*types.ImageSummary)
	}

	// Docker uses different nomenclatures in different places...sometimes a short
	// ID, sometimes long, etc. So we store both in the map so we can always find
	// the same image object. We store the tags and digests, too.
	for i, image := range images {
		data.DockerImages[image.ID[:12]] = &images[i]
		data.DockerImages[image.ID] = &images[i]
		for _, repotag := range image.RepoTags {
			data.DockerImages[repotag] = &images[i]
		}
		for _, repodigest := range image.RepoDigests {
			data.DockerImages[repodigest] = &images[i]
		}
	}

	return nil
}

func pullImage(data *Data, client *client.Client, authConfig *AuthConfigs, image string) error {
	log.Printf("[DEBUG] pulling image: %s", image)

	pullOpts := parseImageOptions(image)

	log.Printf("[DEBUG] Registry: %s", pullOpts.Registry)
	// If a registry was specified in the image name, try to find auth for it
	auth := types.AuthConfig{}
	if pullOpts.Registry != "" {
		if authConfig, ok := authConfig.Configs[normalizeRegistryAddress(pullOpts.Registry)]; ok {
			auth = authConfig
		}
	} else {
		// Try to find an auth config for the public docker hub if a registry wasn't given
		if authConfig, ok := authConfig.Configs["https://registry.hub.docker.com"]; ok {
			auth = authConfig
		}
	}

	encodedJSON, err := json.Marshal(auth)
	if err != nil {
		return fmt.Errorf("error creating auth config: %s", err)
	}

	responseBody, err := client.ImagePull(context.Background(), pullOpts.FqName, types.ImagePullOptions{
		RegistryAuth: base64.URLEncoding.EncodeToString(encodedJSON),
	})
	if err != nil {
		return fmt.Errorf("error pulling image %s: %s", pullOpts.FqName, err)
	}
	defer responseBody.Close()

	pullOutput, err = decodePushPullMessages(responseBody)
	if err != nil {
		return fmt.Errorf("error decoding pull image messages: %s", err)
	}

	log.Printf("[DEBUG] image pull output: %s", pullOutput)

	return nil
}

type internalImageOptions struct {
	Name               string
	FqName             string
	Registry           string
	NormalizedRegistry string
	Repository         string
	Tag                string
}

func parseImageOptions(image string) internalImageOptions {
	pullOpts := internalImageOptions{}

	// Pre-fill with image by default, update later if tag found
	pullOpts.Repository = image

	firstSlash := strings.Index(image, "/")

	// Detect the registry name - it should either contain port, be fully qualified or be localhost
	// If the image contains more than 2 path components, or at least one and the prefix looks like a hostname
	if strings.Count(image, "/") > 1 || firstSlash != -1 && (strings.ContainsAny(image[:firstSlash], ".:") || image[:firstSlash] == "localhost") {
		// registry/repo/image
		pullOpts.Registry = image[:firstSlash]
	}

	prefixLength := len(pullOpts.Registry)
	tagIndex := strings.Index(image[prefixLength:], ":")

	if tagIndex != -1 {
		// we have the tag, strip it
		pullOpts.Repository = image[:prefixLength+tagIndex]
		pullOpts.Tag = image[prefixLength+tagIndex+1:]
	}

	pullOpts.NormalizedRegistry = normalizeRegistryAddress(pullOpts.Registry)
	if pullOpts.Registry == "" {
		pullOpts.FqName = fmt.Sprintf("%s:%s", pullOpts.Repository, pullOpts.Tag)
	} else {
		pullOpts.FqName = fmt.Sprintf("%s/%s:%s", pullOpts.Registry, pullOpts.Repository, pullOpts.Tag)
	}
	return pullOpts
}

func pushImage(client *client.Client, authConfig *AuthConfigs, image string) error {
	log.Printf("[DEBUG] pushing image: %s", image)

	pushOpts := parseImageOptions(image)

	// If a registry was specified in the image name, try to find auth for it
	auth := types.AuthConfig{}
	if pushOpts.Registry != "" {
		if authConfig, ok := authConfig.Configs[normalizeRegistryAddress(pushOpts.Registry)]; ok {
			auth = authConfig
		}
	} else {
		// Try to find an auth config for the public docker hub if a registry wasn't given
		if authConfig, ok := authConfig.Configs["https://registry.hub.docker.com"]; ok {
			auth = authConfig
		}
	}

	encodedJSON, err := json.Marshal(auth)
	if err != nil {
		return fmt.Errorf("error creating auth config: %s", err)
	}

	responseBody, err := client.ImagePush(context.Background(), pushOpts.FqName, types.ImagePushOptions{
		RegistryAuth: base64.URLEncoding.EncodeToString(encodedJSON),
	})

	if err != nil {
		return fmt.Errorf("error pushing image [%s][%s]: %s", image, pushOpts.FqName, err)
	}
	defer responseBody.Close()

	pushOutput, err = decodePushPullMessages(responseBody)
	if err != nil {
		return fmt.Errorf("error decoding push image messages: %s", err)
	}

	return nil
}

func findImage(imageName string, client *client.Client, authConfig *AuthConfigs) (*types.ImageSummary, error) {
	log.Printf("[DEBUG] findImage: [%s]", imageName)

	if imageName == "" {
		return nil, fmt.Errorf("Empty image name is not allowed")
	}

	var data Data
	// load local images into the data structure
	if err := fetchLocalImages(&data, client); err != nil {
		return nil, err
	}

	foundImage := searchLocalImages(data, imageName)
	if foundImage != nil {
		return foundImage, nil
	}

	if err := pullImage(&data, client, authConfig, imageName); err != nil {
		return nil, fmt.Errorf("Unable to pull image %s: %s", imageName, err)
	}

	// update the data structure of the images
	if err := fetchLocalImages(&data, client); err != nil {
		return nil, err
	}

	foundImage = searchLocalImages(data, imageName)
	if foundImage != nil {
		return foundImage, nil
	}

	return nil, fmt.Errorf("Unable to find or pull image %s", imageName)
}

func buildDockerImage(rawBuild map[string]interface{}, imageName string, client *client.Client) (string, error) {
	buildOptions := types.ImageBuildOptions{}

	buildOptions.Version = types.BuilderV1
	buildOptions.Dockerfile = rawBuild["dockerfile"].(string)

	tags := []string{imageName}
	for _, t := range rawBuild["tag"].([]interface{}) {
		tags = append(tags, t.(string))
	}
	buildOptions.Tags = tags

	buildOptions.ForceRemove = rawBuild["force_remove"].(bool)
	buildOptions.Remove = rawBuild["remove"].(bool)
	buildOptions.NoCache = rawBuild["no_cache"].(bool)
	buildOptions.Target = rawBuild["target"].(string)

	buildArgs := make(map[string]*string)
	for k, v := range rawBuild["build_arg"].(map[string]interface{}) {
		val := v.(string)
		buildArgs[k] = &val
	}
	buildOptions.BuildArgs = buildArgs
	log.Printf("[DEBUG] Build Args: %v\n", buildArgs)

	labels := make(map[string]string)
	for k, v := range rawBuild["label"].(map[string]interface{}) {
		labels[k] = v.(string)
	}
	buildOptions.Labels = labels
	log.Printf("[DEBUG] Labels: %v\n", labels)

	contextDir := rawBuild["path"].(string)
	excludes, err := build.ReadDockerignore(contextDir)
	if err != nil {
		return "", err
	}
	excludes = build.TrimBuildFilesFromExcludes(excludes, buildOptions.Dockerfile, false)

	var response types.ImageBuildResponse
	response, err = client.ImageBuild(context.Background(), getBuildContext(contextDir, excludes), buildOptions)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	return decodeBuildMessages(response)
}
