package harbor

import (
	"fmt"
	"io/ioutil"
	"strings"
	"sync"

	"github.com/flanksource/karina/pkg/platform"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
)

const maxHarborImagesChannelLength = 100000
const maxHarborTagsChannelLength = 300000

func ReplicateAll(p *platform.Platform) error {
	client, err := NewClient(p)
	if err != nil {
		return err
	}

	p.Infof("Listing replication policies")
	replications, err := client.ListReplicationPolicies()
	if err != nil {
		return fmt.Errorf("replicateAll: failed to list replication policies: %v", err)
	}
	for _, r := range replications {
		p.Infof("Triggering replication of %s (%d)\n", r.Name, r.ID)
		req, err := client.TriggerReplication(r.ID)
		if err != nil {
			return fmt.Errorf("replicateAll: failed to trigger replication: %v", err)
		}
		p.Infof("%s %s: %s  pending: %d, success: %d, failed: %d\n", req.StartTime, req.Status, req.StatusText, req.InProgress, req.Succeed, req.Failed)
	}
	return nil
}

func UpdateSettings(p *platform.Platform) error {
	client, err := NewClient(p)
	if err != nil {
		return err
	}
	p.Infof("Platform: %v", p)
	p.Infof("Settings: %v", *p.Harbor.Settings)
	return client.UpdateSettings(*p.Harbor.Settings)
}

func ListProjects(p *platform.Platform) ([]Project, error) {
	client, err := NewIngressClient(p)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create harbor client")
	}

	projects, err := client.ListProjects()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to list projects")
	}

	return projects, nil
}

func ListImagesWithTags(p *platform.Platform, concurrency int) (chan Tag, error) {
	client, err := NewIngressClient(p)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create harbor client")
	}

	imagesCh, err := ListImages(p, concurrency)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to list images")
	}

	tagsCh := make(chan Tag, maxHarborTagsChannelLength)

	go func() {
		wg := &sync.WaitGroup{}
		wg.Add(concurrency)

		for i := 0; i < concurrency; i++ {
			go func() {
				defer wg.Done()

				for {
					image, more := <-imagesCh
					if !more {
						break
					}

					artifacts, err := client.ListArtifacts(image.ProjectName, image.Name)
					if err != nil {
						p.Errorf("failed to list artifacts for image %s in project %s: %v", image.Name, image.ProjectName, err)
					}
					p.Tracef("artifacts count for %s/%s: %d\n", image.ProjectName, image.Name, len(artifacts))

					for _, artifact := range artifacts {
						digest := artifact.Digest
						// if strings.HasPrefix(digest, "sha256:") {
						// digest = strings.TrimPrefix(digest, "sha256:")
						// }

						tag := Tag{
							Name:           digest,
							ProjectName:    image.ProjectName,
							RepositoryName: image.Name,
							Digest:         digest,
						}

						tagsCh <- tag

						for _, tag := range artifact.Tags {
							tag := Tag{
								Name:           tag.Name,
								ProjectName:    image.ProjectName,
								RepositoryName: image.Name,
								Digest:         digest,
							}

							tagsCh <- tag
						}
					}
				}
			}()
		}

		wg.Wait()
		close(tagsCh)
	}()

	return tagsCh, nil
}

func ListImages(p *platform.Platform, concurrency int) (chan Image, error) {
	client, err := NewIngressClient(p)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create harbor client")
	}

	projects, err := client.ListProjects()
	if err != nil {
		return nil, errors.Wrap(err, "failed to list projects")
	}

	projectsCh := make(chan string, len(projects))
	imagesCh := make(chan Image, maxHarborImagesChannelLength)

	go func() {
		wg := &sync.WaitGroup{}
		wg.Add(concurrency)

		for i := 0; i < concurrency; i++ {
			go func() {
				defer wg.Done()
				for {
					projectName, more := <-projectsCh
					if !more {
						break
					}

					images, err := client.ListImages(projectName)
					if err != nil {
						p.Errorf("failed to list images in project %s: %v", projectName, err)
					}

					for _, image := range images {
						image.ProjectName = projectName
						parts := strings.SplitN(image.Name, "/", 2)
						image.Name = parts[1]
						imagesCh <- image
					}
				}
			}()
		}

		wg.Wait()
		close(imagesCh)
	}()

	for _, project := range projects {
		projectsCh <- project.Name
	}

	close(projectsCh)

	return imagesCh, nil
}

func IntegrityCheck(p *platform.Platform, concurrency int) (chan Tag, error) {
	client, err := NewIngressClient(p)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create harbor client")
	}

	tagsCh, err := ListImagesWithTags(p, concurrency)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list tags")
	}

	brokenTagsCh := make(chan Tag, maxHarborTagsChannelLength)

	go func() {
		wg := &sync.WaitGroup{}
		wg.Add(concurrency)

		for i := 0; i < concurrency; i++ {
			go func() {
				defer wg.Done()

				for {
					tag, more := <-tagsCh
					if !more {
						break
					}

					p.Debugf("tag: %s/%s:%s\n", tag.ProjectName, tag.RepositoryName, tag.Name)

					_, err := client.GetManifest(tag.ProjectName, tag.RepositoryName, tag.Name)
					if err != nil {
						p.Errorf("failed to get manifest for %s/%s:%s: %v", tag.ProjectName, tag.RepositoryName, tag.Name, err)
					}

					if err != nil {
						brokenTagsCh <- tag
					}
					//  else {
					// fmt.Printf("working tag: %s/%s/%s\n", tag.ProjectName, tag.RepositoryName, tag.Name)
					// }
				}
			}()
		}

		wg.Wait()
		close(brokenTagsCh)
	}()

	return brokenTagsCh, nil
}

func IntegrityCheckFromFile(p *platform.Platform, concurrency int, file string) (chan Tag, error) {
	client, err := NewIngressClient(p)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create harbor client")
	}

	tags, _, err := parseIntegrityCheckFile(file)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse file: %s", file)
	}

	tagsCh := make(chan Tag, maxHarborTagsChannelLength)
	brokenTagsCh := make(chan Tag, maxHarborTagsChannelLength)

	go func() {
		wg := &sync.WaitGroup{}
		wg.Add(concurrency)

		for i := 0; i < concurrency; i++ {
			go func() {
				defer wg.Done()

				for {
					tag, more := <-tagsCh
					if !more {
						break
					}

					p.Debugf("tag: %s/%s:%s\n", tag.ProjectName, tag.RepositoryName, tag.Name)

					_, err := client.GetManifest(tag.ProjectName, tag.RepositoryName, tag.Name)
					if err != nil {
						p.Errorf("failed to get manifest for %s/%s:%s: %v", tag.ProjectName, tag.RepositoryName, tag.Name, err)
					}

					if err != nil {
						brokenTagsCh <- tag
					}
				}
			}()
		}

		wg.Wait()
		close(brokenTagsCh)
	}()

	for _, tag := range tags {
		tagsCh <- tag
	}

	close(tagsCh)

	return brokenTagsCh, nil
}

func CheckManifest(p *platform.Platform, image string) error {
	parts := strings.SplitN(image, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("expecting image %s to have project and image name", image)
	}

	projectName := parts[0]
	parts = strings.SplitN(parts[1], ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("expecting image %s to have a tag", image)
	}

	repositoryName := parts[0]
	tagName := parts[1]

	client, err := NewIngressClient(p)
	if err != nil {
		return errors.Wrap(err, "failed to create harbor client")
	}

	_, err = client.GetManifest(projectName, repositoryName, tagName)
	if err != nil {
		return errors.Wrapf(err, "failed to get manifest for image %s", image)
	}

	return nil
}

func BulkDelete(p *platform.Platform, concurrency int, file string, expectedCount int) error {
	client, err := NewIngressClient(p)
	if err != nil {
		return errors.Wrap(err, "failed to create harbor client")
	}

	tags, count, err := parseIntegrityCheckFile(file)
	if err != nil {
		return errors.Wrapf(err, "failed to parse file: %s", file)
	}

	if expectedCount > 0 && count != expectedCount {
		return errors.Errorf("expected %d tags to be deleted, file contains %d tags", expectedCount, count)
	}

	wg := &sync.WaitGroup{}
	wg.Add(concurrency)

	tagsCh := make(chan Tag, maxHarborTagsChannelLength)

	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()

			for {
				tag, more := <-tagsCh
				if !more {
					break
				}

				if err := client.DeleteTag(tag); err != nil {
					p.Errorf("failed to delete tag: %s/%s:%s %v", tag.ProjectName, tag.RepositoryName, tag.Name, err)
				} else {
					p.Infof("Deleted tag %s/%s:%s", tag.ProjectName, tag.RepositoryName, tag.Name)
				}
			}
		}()
	}

	for _, tag := range tags {
		tagsCh <- tag
	}

	close(tagsCh)

	wg.Wait()

	return nil
}

type IntegrityCheckFile struct {
	Artifacts []Tag `yaml:"artifacts"`
	Count     int   `yaml:"count"`
}

func parseIntegrityCheckFile(filename string) ([]Tag, int, error) {
	bytes, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, 0, errors.Wrapf(err, "failed to read file %s", filename)
	}

	integrityCheckFile := IntegrityCheckFile{}

	if err := yaml.Unmarshal(bytes, &integrityCheckFile); err != nil {
		return nil, 0, errors.Wrapf(err, "failed to unmarshal file")
	}

	return integrityCheckFile.Artifacts, integrityCheckFile.Count, nil
}
