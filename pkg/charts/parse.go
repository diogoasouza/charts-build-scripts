package charts

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/blang/semver"
	"github.com/go-git/go-billy/v5"
	"github.com/rancher/charts-build-scripts/pkg/filesystem"
	"github.com/rancher/charts-build-scripts/pkg/options"
	"github.com/rancher/charts-build-scripts/pkg/path"
	"github.com/rancher/charts-build-scripts/pkg/puller"
	"github.com/sirupsen/logrus"
)

var (
	// ErrRemoteDoesNotExist indicates that the remote does not exist in the current repository
	ErrRemoteDoesNotExist = errors.New("repository does not have any matching remotes")
)

// GetPackages returns all packages found within the repository. If there is a specific package provided, it will return just that Package in the list
func GetPackages(repoRoot string, specificPackage string) ([]*Package, error) {
	var packageList []string
	var err error

	// Parse option or get list of all packages in the repo
	packageList, err = ListPackages(repoRoot, specificPackage)
	if err != nil {
		return nil, fmt.Errorf("encountered error while listing packages: %v", err)
	}

	// Instantiate each package that was requested and return the list
	var packages []*Package
	rootFs := filesystem.GetFilesystem(repoRoot)
	for _, packagePath := range packageList {
		pkg, err := GetPackage(rootFs, packagePath)
		if err != nil {
			return nil, err
		}
		if pkg == nil {
			return nil, fmt.Errorf("packages does not exist in path %s", packagePath)
		}
		packages = append(packages, pkg)
	}
	return packages, nil
}

func ListPackages(repoRoot string, specificPackage string) ([]string, error) {
	var packageList []string
	rootFs := filesystem.GetFilesystem(repoRoot)
	exists, err := filesystem.PathExists(rootFs, path.RepositoryPackagesDir)
	if err != nil || !exists {
		return packageList, err
	}

	listPackages := func(fs billy.Filesystem, dirpath string, isDir bool) error {
		if !isDir {
			return nil
		}
		if len(specificPackage) > 0 {
			packagePrefix := filepath.Join(path.RepositoryPackagesDir, specificPackage)
			if dirpath != packagePrefix && !strings.HasPrefix(dirpath, packagePrefix+"/") {
				logrus.Debugf("ignore %s based on packagePrefix %s", dirpath, packagePrefix)
				// Ignore packages not selected by specificPackage
				return nil
			}
		}
		exists, err := filesystem.PathExists(rootFs, filepath.Join(dirpath, path.PackageOptionsFile))
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		packageName, err := filesystem.MovePath(dirpath, path.RepositoryPackagesDir, "")
		if err != nil {
			return err
		}
		packageList = append(packageList, packageName)
		return nil
	}

	return packageList, filesystem.WalkDir(rootFs, path.RepositoryPackagesDir, listPackages)
}

// GetPackage returns a Package based on the options provided
func GetPackage(rootFs billy.Filesystem, name string) (*Package, error) {
	// Get pkgFs
	packageRoot := filepath.Join(path.RepositoryPackagesDir, name)
	exists, err := filesystem.PathExists(rootFs, packageRoot)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	pkgFs, err := rootFs.Chroot(packageRoot)
	if err != nil {
		return nil, err
	}
	// Get package options from package.yaml
	packageOpt, err := options.LoadPackageOptionsFromFile(pkgFs, path.PackageOptionsFile)
	if err != nil {
		return nil, err
	}
	// version and packageVersion can not exist at the same time although both are optional
	if packageOpt.Version != nil && packageOpt.PackageVersion != nil {
		return nil, fmt.Errorf("cannot have both version and packageVersion at the same time")
	}
	var version *semver.Version
	if packageOpt.Version != nil {
		temp, err := semver.Make(*packageOpt.Version)
		if err != nil {
			return nil, fmt.Errorf("cannot parse version %s as an valid semver: %s", *packageOpt.Version, err)
		}
		version = &temp
	}

	// Get charts
	chart, err := GetChartFromOptions(packageOpt.MainChartOptions)
	if err != nil {
		return nil, err
	}
	var additionalCharts []*AdditionalChart
	for _, additionalChartOptions := range packageOpt.AdditionalChartOptions {
		additionalChart, err := GetAdditionalChartFromOptions(additionalChartOptions)
		if err != nil {
			return nil, err
		}
		additionalCharts = append(additionalCharts, &additionalChart)
	}
	p := Package{
		Chart: chart,

		Name:             name,
		Version:          version,
		PackageVersion:   packageOpt.PackageVersion,
		AdditionalCharts: additionalCharts,
		DoNotRelease:     packageOpt.DoNotRelease,

		fs:     pkgFs,
		rootFs: rootFs,
	}
	return &p, nil
}

// GetChartFromOptions returns a Chart based on the options provided
func GetChartFromOptions(opt options.ChartOptions) (Chart, error) {
	upstream, err := GetUpstream(opt.UpstreamOptions)
	if err != nil {
		return Chart{}, err
	}
	workingDir := opt.WorkingDir
	if len(workingDir) == 0 {
		workingDir = "charts"
	}
	return Chart{
		WorkingDir: workingDir,
		Upstream:   upstream,
	}, nil
}

// GetAdditionalChartFromOptions returns an AdditionalChart based on the options provided
func GetAdditionalChartFromOptions(opt options.AdditionalChartOptions) (AdditionalChart, error) {
	var a AdditionalChart
	if opt.UpstreamOptions != nil && opt.CRDChartOptions != nil {
		return a, fmt.Errorf("invalid additional chart options provided: cannot define both UpstreamOptions and CRDChartOptions")
	}
	if opt.UpstreamOptions == nil && opt.CRDChartOptions == nil {
		return a, fmt.Errorf("cannot parse additional chart options: you must either provide a URL (UpstreamOptions) or provide CRDChartOptions")
	}
	if len(opt.WorkingDir) == 0 {
		return a, fmt.Errorf("cannot have additional chart without working directory")
	}
	if opt.WorkingDir == "charts" {
		return a, fmt.Errorf("working directory for an additional chart cannot be charts")
	}
	a = AdditionalChart{
		WorkingDir: opt.WorkingDir,
	}
	if opt.UpstreamOptions != nil {
		upstream, err := GetUpstream(*opt.UpstreamOptions)
		if err != nil {
			return a, err
		}
		a.Upstream = &upstream
	}
	if opt.CRDChartOptions != nil {
		crdDirectory := opt.CRDChartOptions.CRDDirectory
		if len(crdDirectory) == 0 {
			return a, fmt.Errorf("CRD options must provide a directory to place CRDs within")
		}
		templateDirectory := opt.CRDChartOptions.TemplateDirectory
		if len(templateDirectory) == 0 {
			return a, fmt.Errorf("CRD options must provide a template directory")
		}
		a.CRDChartOptions = &options.CRDChartOptions{
			TemplateDirectory:           templateDirectory,
			CRDDirectory:                crdDirectory,
			AddCRDValidationToMainChart: opt.CRDChartOptions.AddCRDValidationToMainChart,
		}
	}
	return a, nil
}

// GetUpstream returns the appropriate Upstream given the options provided
func GetUpstream(opt options.UpstreamOptions) (puller.Puller, error) {
	if opt.URL == "" {
		return nil, fmt.Errorf("URL is not defined")
	}
	if opt.URL == "local" {
		upstream := Local{}
		return upstream, nil
	}
	if strings.HasPrefix(opt.URL, "packages/") {
		upstream := LocalPackage{
			Name: strings.Split(opt.URL, "/")[1],
		}
		if opt.Subdirectory != nil {
			upstream.Subdirectory = opt.Subdirectory
		}
		return upstream, nil
	}
	if strings.HasSuffix(opt.URL, ".git") {
		upstream, err := puller.GetGithubRepository(opt, nil)
		if err != nil {
			return nil, err
		}
		return upstream, nil
	}
	if strings.HasSuffix(opt.URL, ".tgz") || strings.Contains(opt.URL, ".tar.gz") {
		upstream := puller.Archive{
			URL: opt.URL,
		}
		if opt.Subdirectory != nil {
			upstream.Subdirectory = opt.Subdirectory
		}
		return upstream, nil
	}
	return nil, fmt.Errorf("URL is invalid (must contain .git or .tgz)")
}
