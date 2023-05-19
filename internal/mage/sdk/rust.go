package sdk

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"dagger.io/dagger"
	"github.com/magefile/mage/mg"
	"golang.org/x/sync/errgroup"

	"github.com/dagger/dagger/internal/mage/util"
)

const (
	rustGeneratedAPIPath = "sdk/rust/crates/dagger-sdk/src/gen.rs"
	rustVersionFilePath  = "sdk/rust/crates/dagger-sdk/src/core/mod.rs"
)

var _ SDK = Rust{}

type Rust mg.Namespace

// Bump implements SDK
func (Rust) Bump(ctx context.Context, engineVersion string) error {
	versionStr := `pub const DAGGER_ENGINE_VERSION: &'static str = "([0-9\.-a-zA-Z]+)";`
	versionStrf := `pub const DAGGER_ENGINE_VERSION: &'static str = "%s";`
	version := strings.TrimPrefix(engineVersion, "v")

	versionContents, err := os.ReadFile(rustVersionFilePath)
	if err != nil {
		return err
	}

	versionRe, err := regexp.Compile(versionStr)
	if err != nil {
		return err
	}

	versionBumpedContents := versionRe.ReplaceAll(
		versionContents,
		[]byte(fmt.Sprintf(versionStrf, version)),
	)

	err = os.WriteFile(rustVersionFilePath, versionBumpedContents, 0o600)
	if err != nil {
		return err
	}

	return nil
}

// Generate implements SDK
func (r Rust) Generate(ctx context.Context) error {
	c, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		return err
	}
	defer c.Close()

	c = c.Pipeline("sdk").Pipeline("rust").Pipeline("generate")

	devEngine, endpoint, err := util.CIDevEngineContainerAndEndpoint(
		ctx,
		c.Pipeline("dev-engine"),
		util.DevEngineOpts{Name: "sdk-rust-test"},
	)
	if err != nil {
		return err
	}

	cliBinPath := "/.dagger-cli"

	version := "nightly"
	generated := r.rustBase(ctx, c.Pipeline(version), version).
		WithServiceBinding("dagger-engine", devEngine).
		WithEnvVariable("_EXPERIMENTAL_DAGGER_RUNNER_HOST", endpoint).
		WithMountedFile(cliBinPath, util.DaggerBinary(c)).
		WithEnvVariable("_EXPERIMENTAL_DAGGER_CLI_BIN", cliBinPath).
		WithExec([]string{"cargo", "run", "-p", "dagger-bootstrap", "generate", "--output", fmt.Sprintf("/%s", rustGeneratedAPIPath)}).
		WithExec([]string{"cargo", "fix", "--all", "--allow-no-vcs"}).
		WithExec([]string{"cargo", "fmt"})

	contents, err := generated.File(strings.TrimPrefix(rustGeneratedAPIPath, "sdk/rust/")).
		Contents(ctx)
	if err != nil {
		return err
	}
	if err := os.WriteFile(rustGeneratedAPIPath, []byte(contents), 0o600); err != nil {
		return err
	}

	return nil
}

// Lint implements SDK
func (r Rust) Lint(ctx context.Context) error {
	c, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		return err
	}
	defer c.Close()

	c = c.Pipeline("sdk").Pipeline("rust").Pipeline("lint")

	eg, gctx := errgroup.WithContext(ctx)

	base := r.rustBase(ctx, c, "nightly")

	eg.Go(func() error {
		_, err = base.
			WithExec([]string{"cargo", "check", "--all", "--release"}).
			ExitCode(gctx)

		return err
	})

	eg.Go(func() error {
		_, err = base.
			WithExec([]string{"cargo", "fmt", "--check"}).
			ExitCode(gctx)

		return err
	})

	eg.Go(func() error {
		return lintGeneratedCode(func() error {
			return r.Generate(gctx)
		}, rustGeneratedAPIPath)
	})

	return eg.Wait()
}

// Publish implements SDK
func (Rust) Publish(ctx context.Context, tag string) error {
	panic("unimplemented")
}

// Test implements SDK
func (r Rust) Test(ctx context.Context) error {
	c, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		return err
	}
	defer c.Close()

	c = c.Pipeline("sdk").Pipeline("rust").Pipeline("test")

	devEngine, endpoint, err := util.CIDevEngineContainerAndEndpoint(
		ctx,
		c.Pipeline("dev-engine"),
		util.DevEngineOpts{Name: "sdk-rust-test"},
	)
	if err != nil {
		return err
	}

	cliBinPath := "/.dagger-cli"

	eg, egctx := errgroup.WithContext(ctx)
	for _, version := range []string{
		"stable", "nightly",
	} {
		version := version
		eg.Go(func() error {
			_, err = r.rustBase(egctx, c.Pipeline(version), version).
				WithServiceBinding("dagger-engine", devEngine).
				WithEnvVariable("_EXPERIMENTAL_DAGGER_RUNNER_HOST", endpoint).
				WithMountedFile(cliBinPath, util.DaggerBinary(c)).
				WithEnvVariable("_EXPERIMENTAL_DAGGER_CLI_BIN", cliBinPath).
				WithExec([]string{"cargo", "test", "--release", "--all"}).
				ExitCode(ctx)
			return err
		})
	}

	return eg.Wait()
}

func (Rust) rustBase(ctx context.Context, c *dagger.Client, tag string) *dagger.Container {
	const (
		appDir = "sdk/rust"
	)

	src := c.Directory().WithDirectory("/", util.Repository(c).Directory(appDir))

	mountPath := fmt.Sprintf("/%s", appDir)

	base := c.
		Container().
		From(fmt.Sprintf("rustlang/rust:%s", tag)).
		WithMountedCache("~/.cargo", c.CacheVolume("rust-cargo")).
		WithExec([]string{"cargo", "install", "cargo-chef"}).
		WithWorkdir(mountPath).
		WithDirectory(mountPath, src, dagger.ContainerWithDirectoryOpts{
			Include: []string{
				"**/Cargo.toml",
				"**/Cargo.lock",
				"**/main.rs",
				"**/lib.rs",
			},
		}).
		WithExec([]string{
			"mkdir", "-p", "/mnt/recipe",
		}).
		WithMountedCache("/mnt/recipe", c.CacheVolume("rust-chef-recipe")).
		WithExec([]string{
			"cargo", "chef", "prepare", "--recipe-path", "/mnt/recipe/recipe.json",
		}).
		WithMountedCache(fmt.Sprintf("%s/target", mountPath), c.CacheVolume("rust-target")).
		WithExec([]string{
			"cargo", "chef", "cook", "--release", "--workspace", "--recipe-path", "/mnt/recipe/recipe.json",
		}).
		WithMountedDirectory(mountPath, src)

	return base
}
