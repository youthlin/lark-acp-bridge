// Package update 实现 lark-acp-bridge 二进制的自更新：
// 查询 GitHub Release 最新版本、比较版本号、下载对应平台 tar.gz、
// 校验 sha256、解压并原子替换当前可执行文件。
package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 默认发布仓库；可通过 Options.Repo 覆盖。
const DefaultRepo = "youthlin/lark-acp-bridge"

// binaryName 返回当前平台的产物二进制名。
func binaryName(goos string) string {
	if goos == "windows" {
		return "lark-acp-bridge.exe"
	}
	return "lark-acp-bridge"
}

// assetName 按 release.yml 的命名规则拼出压缩包名。
func assetName(version, goos, goarch string) string {
	return fmt.Sprintf("lark-acp-bridge_%s_%s_%s.tar.gz", version, goos, goarch)
}

// DefaultGiteeRepo 是 Gitee 镜像仓库，GitHub 不可达时作为更新回退源。
const DefaultGiteeRepo = "youthlin/lark-acp-bridge"

// Mirror 是一个备用下载源（资产与对应 sha256 的 URL）。
type Mirror struct {
	Name      string
	AssetURL  string
	Sha256URL string
}

// Release 描述一个可用版本。
type Release struct {
	Tag        string
	AssetName  string
	AssetURL   string
	Sha256URL  string
	Body       string
	Prerelease bool
	// Mirrors 是 GitHub 主源之外的备用下载源（如 Gitee Release），下载失败时按顺序尝试。
	Mirrors []Mirror
}

// Options 控制更新行为。
type Options struct {
	// CurrentVersion 是当前二进制版本（来自 main.version 或 build info）。
	CurrentVersion string
	// GOOS/GOARCH 默认为 runtime 值，测试时可覆盖。
	GOOS   string
	GOARCH string
	// Repo 形如 "owner/name"，默认 DefaultRepo。
	Repo string
	// GiteeRepo 是 Gitee 镜像仓库，形如 "owner/name"；为空则用 DefaultGiteeRepo。
	// 设为 "-" 可禁用 Gitee 回退。
	GiteeRepo string
	// HTTPClient 可覆盖；默认使用带超时的 client。
	HTTPClient *http.Client
	// ExePath 是待替换的可执行文件路径，默认 os.Executable()。
	ExePath string
}

func (o *Options) normalize() {
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.GOARCH == "" {
		o.GOARCH = runtime.GOARCH
	}
	if o.Repo == "" {
		o.Repo = DefaultRepo
	}
	if o.GiteeRepo == "" {
		o.GiteeRepo = DefaultGiteeRepo
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
}

// LatestRelease 查询仓库的最新正式 Release。
// 优先调用 GitHub API（若存在 GITHUB_TOKEN 则附带）；API 不可用时回退到
// 请求 https://github.com/<repo>/releases/latest 的 302 重定向来解析 tag，
// 资产名按发布规则直接拼出，避免受 API 限流影响。
func (o *Options) LatestRelease(ctx context.Context) (*Release, error) {
	o.normalize()
	// 依次尝试：GitHub API → Gitee API → GitHub 重定向。
	// 每个来源给一个独立的短超时，避免国内网络下长时间卡住。
	type candidate struct {
		name string
		fn   func(context.Context) (*Release, error)
	}
	candidates := []candidate{
		{"github-api", o.latestViaAPI},
	}
	if o.GiteeRepo != "-" {
		candidates = append(candidates, candidate{"gitee-api", o.latestViaGitee})
	}
	candidates = append(candidates, candidate{"github-redirect", func(ctx context.Context) (*Release, error) {
		tag, err := o.latestTagViaRedirect(ctx)
		if err != nil {
			return nil, err
		}
		return o.releaseForTag(tag, ""), nil
	}})

	var errs []string
	for _, c := range candidates {
		rel, err := func() (*Release, error) {
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			return c.fn(cctx)
		}()
		if err == nil {
			return rel, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", c.name, err))
	}
	return nil, fmt.Errorf("查询最新版本失败: %s", strings.Join(errs, "; "))
}

type giteeRelease struct {
	TagName    string `json:"tag_name"`
	Name       string `json:"name"`
	Body       string `json:"body"`
	Prerelease bool   `json:"prerelease"`
}

// latestViaGitee 通过 Gitee OpenAPI v5 查询最新 Release。
func (o *Options) latestViaGitee(ctx context.Context) (*Release, error) {
	repo := strings.TrimSpace(o.GiteeRepo)
	if repo == "" {
		repo = DefaultGiteeRepo
	}
	if repo == "-" {
		return nil, errors.New("Gitee 镜像已禁用")
	}
	url := fmt.Sprintf("https://gitee.com/api/v5/repos/%s/releases/latest", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if tok := strings.TrimSpace(os.Getenv("GITEE_TOKEN")); tok != "" {
		q := req.URL.Query()
		q.Set("access_token", tok)
		req.URL.RawQuery = q.Encode()
	}
	resp, err := o.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Gitee API 返回 %s", resp.Status)
	}
	var rel giteeRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, errors.New("Gitee 暂无 Release")
	}
	return o.releaseForTag(rel.TagName, rel.Body), nil
}

type ghRelease struct {
	TagName    string `json:"tag_name"`
	Name       string `json:"name"`
	Body       string `json:"body"`
	Prerelease bool   `json:"prerelease"`
}

func (o *Options) latestViaAPI(ctx context.Context) (*Release, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", o.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := o.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API 返回 %s", resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, errors.New("GitHub API 返回空 tag_name")
	}
	return o.releaseForTag(rel.TagName, rel.Body), nil
}

// latestTagViaRedirect 请求 /releases/latest，通过 302 Location 解析最新 tag。
func (o *Options) latestTagViaRedirect(ctx context.Context) (string, error) {
	url := fmt.Sprintf("https://github.com/%s/releases/latest", o.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	// 不自动跟随重定向，从 Location 取 tag。
	client := *o.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusMovedPermanently {
		return "", fmt.Errorf("GitHub 返回 %s", resp.Status)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", errors.New("GitHub 未返回 Location")
	}
	// Location 形如 https://github.com/owner/repo/releases/tag/v1.2.3
	idx := strings.LastIndex(loc, "/")
	if idx < 0 || idx == len(loc)-1 {
		return "", fmt.Errorf("无法从 Location 解析 tag: %s", loc)
	}
	tag := loc[idx+1:]
	if tag == "" {
		return "", fmt.Errorf("无法从 Location 解析 tag: %s", loc)
	}
	return tag, nil
}

func (o *Options) releaseForTag(tag, body string) *Release {
	name := assetName(tag, o.GOOS, o.GOARCH)
	ghBase := fmt.Sprintf("https://github.com/%s/releases/download/%s/", o.Repo, tag)
	rel := &Release{
		Tag:       tag,
		AssetName: name,
		AssetURL:  ghBase + name,
		Sha256URL: ghBase + name + ".sha256",
		Body:      body,
	}
	// Gitee 镜像回退源（可通过 GiteeRepo == "-" 禁用）。
	gitee := strings.TrimSpace(o.GiteeRepo)
	if gitee == "" {
		gitee = DefaultGiteeRepo
	}
	if gitee != "-" {
		giteeBase := fmt.Sprintf("https://gitee.com/%s/releases/download/%s/", gitee, tag)
		rel.Mirrors = append(rel.Mirrors, Mirror{
			Name:      "gitee",
			AssetURL:  giteeBase + name,
			Sha256URL: giteeBase + name + ".sha256",
		})
	}
	return rel
}

// ReleaseForVersion 按发布命名规则为指定 tag 构造 Release，不发起网络请求。
func (o *Options) ReleaseForVersion(tag string) *Release {
	o.normalize()
	return o.releaseForTag(tag, "")
}

// IsNewer 比较候选版本是否比当前版本新。
// 当前版本为 "dev" 或空时，任何具体版本都视为更新。
func IsNewer(current, latest string) bool {
	cur := strings.TrimSpace(current)
	lat := strings.TrimSpace(latest)
	if cur == "" || cur == "dev" {
		return lat != "" && lat != cur
	}
	return compareVersion(lat, cur) > 0
}

// compareVersion 宽松比较两个版本号：去除前导 v，按 ./- 分段，
// 数值段按整数比较，非数值段按字符串比较；更长的前置相同版本更大。
func compareVersion(a, b string) int {
	pa := segments(a)
	pb := segments(b)
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var sa, sb string
		if i < len(pa) {
			sa = pa[i]
		}
		if i < len(pb) {
			sb = pb[i]
		}
		if sa == sb {
			continue
		}
		na, erra := strconv.Atoi(sa)
		nb, errb := strconv.Atoi(sb)
		switch {
		case erra == nil && errb == nil:
			if na != nb {
				return intCmp(na, nb)
			}
		case erra == nil: // 数值段 > 非数值段，a 更新
			return 1
		case errb == nil:
			return -1
		default:
			if sa != sb {
				return strings.Compare(sa, sb)
			}
		}
	}
	return 0
}

func segments(v string) []string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.ReplaceAll(v, "-", ".")
	raw := strings.Split(v, ".")
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func intCmp(a, b int) int {
	switch {
	case a > b:
		return 1
	case a < b:
		return -1
	default:
		return 0
	}
}

// UpdateResult 描述一次更新结果。
type UpdateResult struct {
	From       string
	To         string
	ExePath    string
	BackupPath string
	// Source 是实际下载成功的源名（github / gitee ...）。
	Source string
}

type RollbackResult struct {
	ExePath    string
	BackupPath string
	Size       int64
	SHA256     string
}

// Apply 下载指定 Release，校验 sha256 并替换当前可执行文件。
// 不负责重启服务；调用方应在成功后提示用户重启。
func (o *Options) Apply(ctx context.Context, rel *Release) (*UpdateResult, error) {
	o.normalize()
	if rel == nil {
		return nil, errors.New("没有可用的更新版本")
	}
	exePath := o.ExePath
	if exePath == "" {
		var err error
		exePath, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("定位当前可执行文件失败: %w", err)
		}
	}
	resolved, err := filepath.EvalSymlinks(exePath)
	if err == nil {
		exePath = resolved
	}

	// 组装候选下载源：GitHub 主源在前，mirrors 在后。
	type source struct {
		name      string
		assetURL  string
		sha256URL string
	}
	candidates := []source{{name: "github", assetURL: rel.AssetURL, sha256URL: rel.Sha256URL}}
	for _, m := range rel.Mirrors {
		if strings.TrimSpace(m.AssetURL) == "" {
			continue
		}
		candidates = append(candidates, source{name: m.Name, assetURL: m.AssetURL, sha256URL: m.Sha256URL})
	}

	var (
		data     []byte
		lastErr  error
		usedFrom string
	)
	for _, c := range candidates {
		wantHash, err := o.fetchSha256(ctx, c.sha256URL)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", c.name, err)
			continue
		}
		data, err = o.download(ctx, c.assetURL, wantHash)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", c.name, err)
			continue
		}
		usedFrom = c.name
		lastErr = nil
		break
	}
	if lastErr != nil {
		return nil, fmt.Errorf("所有下载源均失败: %w", lastErr)
	}

	bin, err := extractBinary(data, binaryName(o.GOOS))
	if err != nil {
		return nil, err
	}

	if err := replaceExecutable(exePath, bin, o.GOOS); err != nil {
		return nil, err
	}
	return &UpdateResult{From: o.CurrentVersion, To: rel.Tag, ExePath: exePath, BackupPath: backupPathFor(exePath), Source: usedFrom}, nil
}

// Rollback 恢复最近一次 Apply 保存的备份。不负责重启服务。
func (o *Options) Rollback(ctx context.Context) (*RollbackResult, error) {
	o.normalize()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	exePath := o.ExePath
	if exePath == "" {
		var err error
		exePath, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("定位当前可执行文件失败: %w", err)
		}
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	backupPath := backupPathFor(exePath)
	if err := validateExecutableFile(exePath, "目标文件"); err != nil {
		return nil, err
	}
	if err := validateExecutableFile(backupPath, "备份文件"); err != nil {
		return nil, err
	}
	backupSHA, backupSize, err := fileSHA256AndSize(backupPath)
	if err != nil {
		return nil, fmt.Errorf("校验备份文件失败: %w", err)
	}
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return nil, fmt.Errorf("读取备份文件失败: %w", err)
	}
	if err := replaceExecutableContent(exePath, data, 0o755, o.GOOS); err != nil {
		return nil, err
	}
	if err := validateExecutableFile(exePath, "目标文件"); err != nil {
		return nil, err
	}
	restoredSHA, restoredSize, err := fileSHA256AndSize(exePath)
	if err != nil {
		return nil, fmt.Errorf("校验恢复后的目标文件失败: %w", err)
	}
	if restoredSHA != backupSHA || restoredSize != backupSize {
		return nil, fmt.Errorf("回滚校验失败: 备份 sha256=%s size=%d, 目标 sha256=%s size=%d", backupSHA, backupSize, restoredSHA, restoredSize)
	}
	return &RollbackResult{ExePath: exePath, BackupPath: backupPath, Size: restoredSize, SHA256: restoredSHA}, nil
}

func (o *Options) fetchSha256(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := o.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载校验文件失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载校验文件失败: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	// sha256sum 输出形如 "<hex>  filename"，取首段。
	fields := strings.Fields(strings.TrimSpace(string(body)))
	if len(fields) == 0 {
		return "", errors.New("校验文件为空")
	}
	hash := strings.ToLower(fields[0])
	if len(hash) != 64 {
		return "", fmt.Errorf("校验文件中的 sha256 长度异常: %q", hash)
	}
	return hash, nil
}

func (o *Options) download(ctx context.Context, url, wantHash string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载更新包失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载更新包失败: %s", resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取更新包失败: %w", err)
	}
	sum := sha256.Sum256(data)
	gotHash := hex.EncodeToString(sum[:])
	if !strings.EqualFold(gotHash, wantHash) {
		return nil, fmt.Errorf("sha256 校验失败: 期望 %s, 实际 %s", wantHash, gotHash)
	}
	return data, nil
}

// extractBinary 从 gzip tar 数据中解出指定名称的文件内容。
// 压缩包可能带有 README/LICENSE 等额外文件，仅提取目标二进制。
func extractBinary(data []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("解压 gzip 失败: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取 tar 失败: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(hdr.Name) == name {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("压缩包中未找到二进制 %s", name)
}

// replaceExecutable 原子替换目标可执行文件。
// Unix: 写临时文件 -> chmod -> rename 覆盖（允许替换正在运行的程序）。
// Windows: 先把旧文件重命名为 .old，再写入新文件；旧文件留待重启删除。
func replaceExecutable(target string, content []byte, goos string) error {
	if err := backupExecutable(target); err != nil {
		return err
	}
	return replaceExecutableContent(target, content, 0o755, goos)
}

func replaceExecutableContent(target string, content []byte, perm fs.FileMode, goos string) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".lark-acp-bridge-update-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("设置可执行权限失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}

	if goos == "windows" {
		oldPath := target + ".old"
		_ = os.Remove(oldPath)
		if err := os.Rename(target, oldPath); err != nil && !os.IsNotExist(err) {
			cleanup()
			return fmt.Errorf("备份旧版本失败: %w", err)
		}
		if err := os.Rename(tmpPath, target); err != nil {
			// 尝试回滚。
			_ = os.Rename(oldPath, target)
			cleanup()
			return fmt.Errorf("替换可执行文件失败: %w", err)
		}
		_ = os.Remove(oldPath) // 删不掉也无妨，下次覆盖。
		return nil
	}

	if err := os.Rename(tmpPath, target); err != nil {
		cleanup()
		return fmt.Errorf("替换可执行文件失败: %w", err)
	}
	return nil
}

func backupPathFor(target string) string {
	return target + ".bak"
}

func backupExecutable(target string) error {
	if err := validateExecutableFile(target, "当前可执行文件"); err != nil {
		return err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return fmt.Errorf("读取当前可执行文件失败: %w", err)
	}
	backupPath := backupPathFor(target)
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("读取当前可执行文件状态失败: %w", err)
	}
	if err := replaceExecutableContent(backupPath, data, info.Mode().Perm()|0o111, runtime.GOOS); err != nil {
		return fmt.Errorf("保存旧版本备份失败: %w", err)
	}
	if err := validateExecutableFile(backupPath, "备份文件"); err != nil {
		return err
	}
	return nil
}

func validateExecutableFile(path, label string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) && label == "备份文件" {
			return fmt.Errorf("没有可回滚的备份文件: %s", path)
		}
		return fmt.Errorf("%s不可用: %s: %w", label, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s不是普通文件: %s", label, path)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("%s缺少可执行权限: %s", label, path)
	}
	return nil
}

func fileSHA256AndSize(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

// SortReleases 按版本号降序排序（最新在前），原地排序。
func SortReleases(rels []*Release) {
	sort.Slice(rels, func(i, j int) bool {
		return compareVersion(rels[i].Tag, rels[j].Tag) > 0
	})
}
