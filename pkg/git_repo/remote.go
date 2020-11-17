package git_repo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/ini.v1"

	"github.com/werf/werf/pkg/true_git"
	"github.com/werf/werf/pkg/werf"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/werf/lockgate"
	"github.com/werf/logboek"
)

type Remote struct {
	Base
	Url      string
	IsDryRun bool

	Endpoint *transport.Endpoint
}

func OpenRemoteRepo(name, url string) (*Remote, error) {
	repo := &Remote{
		Base: Base{Name: name, GitDataManager: NewCommonGitDataManager()},
		Url:  url,
	}
	return repo, repo.ValidateEndpoint()
}

func (repo *Remote) ValidateEndpoint() error {
	if ep, err := transport.NewEndpoint(repo.Url); err != nil {
		return fmt.Errorf("bad url '%s': %s", repo.Url, err)
	} else {
		repo.Endpoint = ep
	}
	return nil
}

func (repo *Remote) CreateDetachedMergeCommit(ctx context.Context, fromCommit, toCommit string) (string, error) {
	return repo.createDetachedMergeCommit(ctx, repo.GetClonePath(), repo.GetClonePath(), repo.getWorkTreeCacheDir(repo.getRepoID()), fromCommit, toCommit)
}

func (repo *Remote) GetMergeCommitParents(_ context.Context, commit string) ([]string, error) {
	return repo.getMergeCommitParents(repo.GetClonePath(), commit)
}

func (repo *Remote) getFilesystemRelativePathByEndpoint() string {
	host := repo.Endpoint.Host
	if repo.Endpoint.Port > 0 {
		host += fmt.Sprintf(":%d", repo.Endpoint.Port)
	}
	return filepath.Join(fmt.Sprintf("protocol-%s", repo.Endpoint.Protocol), host, repo.Endpoint.Path)
}

func (repo *Remote) GetClonePath() string {
	return filepath.Join(GetGitRepoCacheDir(), repo.getFilesystemRelativePathByEndpoint())
}

func (repo *Remote) RemoteOriginUrl() (string, error) {
	return repo.remoteOriginUrl(repo.GetClonePath())
}

func (repo *Remote) IsEmpty(ctx context.Context) (bool, error) {
	return repo.isEmpty(ctx, repo.GetClonePath())
}

func (repo *Remote) IsAncestor(ctx context.Context, ancestorCommit, descendantCommit string) (bool, error) {
	return true_git.IsAncestor(ancestorCommit, descendantCommit, repo.GetClonePath())
}

func (repo *Remote) CloneAndFetch(ctx context.Context) error {
	isCloned, err := repo.Clone(ctx)
	if err != nil {
		return err
	}
	if isCloned {
		return nil
	}

	return repo.Fetch(ctx)
}

func (repo *Remote) isCloneExists() (bool, error) {
	_, err := os.Stat(repo.GetClonePath())
	if err == nil {
		return true, nil
	}

	if !os.IsNotExist(err) {
		return false, fmt.Errorf("cannot clone git repo: %s", err)
	}

	return false, nil
}

func (repo *Remote) Clone(ctx context.Context) (bool, error) {
	if repo.IsDryRun {
		return false, nil
	}

	var err error

	exists, err := repo.isCloneExists()
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}

	return true, repo.withRemoteRepoLock(ctx, func() error {
		exists, err := repo.isCloneExists()
		if err != nil {
			return err
		}
		if exists {
			return nil
		}

		logboek.Context(ctx).Default().LogFDetails("Clone %s\n", repo.Url)

		if err := os.MkdirAll(filepath.Dir(repo.GetClonePath()), 0755); err != nil {
			return fmt.Errorf("unable to create dir %s: %s", filepath.Dir(repo.GetClonePath()), err)
		}

		tmpPath := fmt.Sprintf("%s.tmp", repo.GetClonePath())
		// Remove previously created possibly existing dir
		if err := os.RemoveAll(tmpPath); err != nil {
			return fmt.Errorf("unable to prepare tmp path %s: failed to remove: %s", tmpPath, err)
		}
		// Ensure cleanup on failure
		defer os.RemoveAll(tmpPath)

		_, err = git.PlainClone(tmpPath, true, &git.CloneOptions{
			URL:               repo.Url,
			RecurseSubmodules: git.DefaultSubmoduleRecursionDepth,
		})
		if err != nil {
			return err
		}

		if err := os.Rename(tmpPath, repo.GetClonePath()); err != nil {
			return fmt.Errorf("rename %s to %s failed: %s", tmpPath, repo.GetClonePath(), err)
		}

		return nil
	})
}

func (repo *Remote) Fetch(ctx context.Context) error {
	if repo.IsDryRun {
		return nil
	}

	cfgPath := filepath.Join(repo.GetClonePath(), "config")

	cfg, err := ini.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("cannot load repo `%s` config: %s", repo.String(), err)
	}

	remoteName := "origin"

	oldUrlKey := cfg.Section(fmt.Sprintf("remote \"%s\"", remoteName)).Key("url")
	if oldUrlKey != nil && oldUrlKey.Value() != repo.Url {
		oldUrlKey.SetValue(repo.Url)
		err := cfg.SaveTo(cfgPath)
		if err != nil {
			return fmt.Errorf("cannot update url of repo `%s`: %s", repo.String(), err)
		}
	}

	return repo.withRemoteRepoLock(ctx, func() error {
		rawRepo, err := git.PlainOpenWithOptions(repo.GetClonePath(), &git.PlainOpenOptions{EnableDotGitCommonDir: true})
		if err != nil {
			return fmt.Errorf("cannot open repo: %s", err)
		}

		logboek.Context(ctx).Default().LogFDetails("Fetch remote %s of %s\n", remoteName, repo.Url)

		err = rawRepo.Fetch(&git.FetchOptions{RemoteName: remoteName, Force: true, Tags: git.AllTags})
		if err != nil && err != git.NoErrAlreadyUpToDate {
			return fmt.Errorf("cannot fetch remote `%s` of repo `%s`: %s", remoteName, repo.String(), err)
		}

		return nil
	})
}

func (repo *Remote) HeadCommit(_ context.Context) (string, error) {
	return repo.getHeadCommit(repo.GetClonePath())
}

func (repo *Remote) findReference(rawRepo *git.Repository, reference string) (string, error) {
	refs, err := rawRepo.References()
	if err != nil {
		return "", err
	}

	var res string

	err = refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name().String() == reference {
			res = fmt.Sprintf("%s", ref.Hash())
			return storer.ErrStop
		}

		return nil
	})
	if err != nil {
		return "", err
	}

	return res, nil
}

func (repo *Remote) LatestBranchCommit(ctx context.Context, branch string) (string, error) {
	var err error

	rawRepo, err := git.PlainOpenWithOptions(repo.GetClonePath(), &git.PlainOpenOptions{EnableDotGitCommonDir: true})
	if err != nil {
		return "", fmt.Errorf("cannot open repo: %s", err)
	}

	res, err := repo.findReference(rawRepo, fmt.Sprintf("refs/remotes/origin/%s", branch))
	if err != nil {
		return "", err
	}
	if res == "" {
		return "", fmt.Errorf("unknown branch `%s` of repo `%s`", branch, repo.String())
	}

	logboek.Context(ctx).Info().LogF("Using commit '%s' of repo '%s' branch '%s'\n", res, repo.String(), branch)

	return res, nil
}

func (repo *Remote) TagCommit(ctx context.Context, tag string) (string, error) {
	var err error

	rawRepo, err := git.PlainOpenWithOptions(repo.GetClonePath(), &git.PlainOpenOptions{EnableDotGitCommonDir: true})
	if err != nil {
		return "", fmt.Errorf("cannot open repo: %s", err)
	}

	ref, err := rawRepo.Tag(tag)
	if err != nil {
		return "", fmt.Errorf("bad tag '%s' of repo %s: %s", tag, repo.String(), err)
	}

	var res string

	obj, err := rawRepo.TagObject(ref.Hash())
	switch err {
	case nil:
		// Tag object present
		res = obj.Target.String()
	case plumbing.ErrObjectNotFound:
		res = ref.Hash().String()
	default:
		return "", fmt.Errorf("bad tag '%s' of repo %s: %s", tag, repo.String(), err)
	}

	logboek.Context(ctx).Info().LogF("Using commit '%s' of repo '%s' tag '%s'\n", res, repo.String(), tag)

	return res, nil
}

func (repo *Remote) CreatePatch(ctx context.Context, opts PatchOptions) (Patch, error) {
	return repo.createPatch(ctx, repo.GetClonePath(), repo.GetClonePath(), repo.getRepoID(), repo.getWorkTreeCacheDir(repo.getRepoID()), opts)
}

func (repo *Remote) CreateArchive(ctx context.Context, opts ArchiveOptions) (Archive, error) {
	return repo.createArchive(ctx, repo.GetClonePath(), repo.GetClonePath(), repo.getRepoID(), repo.getWorkTreeCacheDir(repo.getRepoID()), opts)
}

func (repo *Remote) Checksum(ctx context.Context, opts ChecksumOptions) (checksum Checksum, err error) {
	logboek.Context(ctx).Debug().LogProcess("Calculating checksum").Do(func() {
		checksum, err = repo.checksumWithLsTree(ctx, repo.GetClonePath(), repo.GetClonePath(), repo.getWorkTreeCacheDir(repo.getRepoID()), opts)
	})

	return checksum, err
}

func (repo *Remote) IsCommitExists(ctx context.Context, commit string) (bool, error) {
	return repo.isCommitExists(ctx, repo.GetClonePath(), repo.GetClonePath(), commit)
}

func (repo *Remote) getRepoID() string {
	return repo.getFilesystemRelativePathByEndpoint()
}

func (repo *Remote) getWorkTreeCacheDir(repoID string) string {
	return filepath.Join(GetWorkTreeCacheDir(), repoID)
}

func (repo *Remote) withRemoteRepoLock(ctx context.Context, f func() error) error {
	lockName := fmt.Sprintf("remote_git_mapping.%s", repo.Name)
	return werf.WithHostLock(ctx, lockName, lockgate.AcquireOptions{Timeout: 600 * time.Second}, f)
}

func (repo *Remote) TagsList(_ context.Context) ([]string, error) {
	return repo.tagsList(repo.GetClonePath())
}

func (repo *Remote) RemoteBranchesList(_ context.Context) ([]string, error) {
	return repo.remoteBranchesList(repo.GetClonePath())
}
