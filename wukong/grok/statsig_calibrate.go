package grok

import (
	"context"
	"fmt"
	"sort"
)

// grok.com 每次前端发版都会换掉 botoxSign 里的 meta 下标字面量，签名随之失效，
// 表现为 /rest/app-chat/conversations/new 返回 403 code 7。这里把「用真实浏览器
// 签名反解下标」的求解过程固化为包内 API，避免和运行时实现漂移。

// StatsigHashKeyword 是 botoxSign 拼接进 SHA-256 原文的固定串。校准工具靠它在
// crypto.subtle.digest 的入参里筛出签名调用，并切出其后的 animationKey。
const StatsigHashKeyword = statsigKeyword

// StatsigCalibrationSample 是一次真实页面加载的观测：页面的 grok-site-verification
// meta，以及浏览器为该次加载算出的 animationKey（从喂给 SHA-256 的原文里截取
// statsigKeyword 之后的部分）。同一前端版本内 Botox 曲线跨页面加载稳定，因此多个
// 样本可以共用一份曲线。
type StatsigCalibrationSample struct {
	Meta         string
	AnimationKey string
}

// StatsigIndexSolution 是一组能复现全部样本的 meta 字节下标。
// TimeA/TimeB/TimeC 只以乘积参与计算，顺序不可区分，求解结果按升序归一。
type StatsigIndexSolution struct {
	SVG   int
	Row   int
	TimeA int
	TimeB int
	TimeC int
}

func (s StatsigIndexSolution) String() string {
	return fmt.Sprintf("%d/%d/%d,%d,%d", s.SVG, s.Row, s.TimeA, s.TimeB, s.TimeC)
}

func (s StatsigIndexSolution) indices() statsigByteIndices {
	return statsigByteIndices{SVG: s.SVG, Row: s.Row, TimeA: s.TimeA, TimeB: s.TimeB, TimeC: s.TimeC, Source: "solved"}
}

func (s StatsigIndexSolution) canonical() StatsigIndexSolution {
	times := []int{s.TimeA, s.TimeB, s.TimeC}
	sort.Ints(times)
	return StatsigIndexSolution{SVG: s.SVG, Row: s.Row, TimeA: times[0], TimeB: times[1], TimeC: times[2]}
}

// Equivalent 比较两组下标是否表示同一组字节。时间下标只以乘积参与计算，顺序无关。
func (s StatsigIndexSolution) Equivalent(other StatsigIndexSolution) bool {
	return s.canonical() == other.canonical()
}

// StatsigIndexDiscovery 是纯 HTTP 从页面或签名器 JS 抽出的下标。
type StatsigIndexDiscovery struct {
	Solution StatsigIndexSolution
	Source   string
	Fetches  int
}

// CurrentStatsigByteIndices 返回当前编译进二进制的默认下标，用于判断线上是否已改版。
func CurrentStatsigByteIndices() StatsigIndexSolution {
	idx := defaultStatsigByteIndices()
	return StatsigIndexSolution{SVG: idx.SVG, Row: idx.Row, TimeA: idx.TimeA, TimeB: idx.TimeB, TimeC: idx.TimeC}
}

// DiscoverStatsigByteIndices 不启动浏览器：下载页面引用的脚本，跟完
// botoxSign → 模块 ID → chunk 映射 → 懒加载签名器，再从明文下标字面量抽出结果。
// TimeA/B/C 按升序归一，便于和当前默认值比较。
func DiscoverStatsigByteIndices(ctx context.Context, pageHTML []byte, pageURL string, fetch func(context.Context, string) ([]byte, error)) (StatsigIndexDiscovery, bool) {
	idx, fetches, ok := discoverStatsigIndicesWithFetch(ctx, pageHTML, pageURL, fetch)
	if !ok {
		return StatsigIndexDiscovery{Fetches: fetches}, false
	}
	return StatsigIndexDiscovery{
		Solution: StatsigIndexSolution{SVG: idx.SVG, Row: idx.Row, TimeA: idx.TimeA, TimeB: idx.TimeB, TimeC: idx.TimeC}.canonical(),
		Source:   idx.Source,
		Fetches:  fetches,
	}, true
}

// SolveStatsigByteIndices 穷举全部下标组合，返回能同时复现所有样本的解。
//
// 直接对 48^2 × C(48,3) ≈ 4000 万种组合逐一跑动画会很慢，但 animationKey 只取决于
// (svg, row, 三个时间字节模 16 的乘积)，所以先把 4 × 16 × 3376 种结果打成表，
// 穷举时退化为数组查表，整体在一秒内完成。
//
// 样本太少时解不唯一：单个样本通常留下成千上万个候选，实测 3 个及以上样本即可收敛
// 到唯一解。调用方应对返回多解的情况要求补充样本。
func SolveStatsigByteIndices(pageHTML []byte, samples []StatsigCalibrationSample) ([]StatsigIndexSolution, error) {
	if len(samples) == 0 {
		return nil, fmt.Errorf("没有可用于反解的样本")
	}
	frames, err := extractStatsigCurves(pageHTML)
	if err != nil {
		return nil, fmt.Errorf("解析 Botox 曲线: %w", err)
	}
	if err := validateStatsigFrames(frames); err != nil {
		return nil, err
	}

	metas := make([][]byte, len(samples))
	for i, sample := range samples {
		raw, decodeErr := decodeStatsigMetaBytes(sample.Meta)
		if decodeErr != nil {
			return nil, fmt.Errorf("样本 %d 的 meta 无效: %w", i, decodeErr)
		}
		if sample.AnimationKey == "" {
			return nil, fmt.Errorf("样本 %d 缺少 animationKey", i)
		}
		metas[i] = raw
	}

	const maxProduct = 15 * 15 * 15
	table := make([][][]string, statsigCurveGroupCount)
	for svg := range table {
		table[svg] = make([][]string, statsigCurveRowCount)
		for row := range table[svg] {
			table[svg][row] = make([]string, maxProduct+1)
			for product := 0; product <= maxProduct; product++ {
				frameTime := jsRound(float64(product)/10) * 10
				table[svg][row][product] = animateStatsigRow(frames[svg][row], frameTime/statsigAnimationTotal)
			}
		}
	}

	matches := func(svgIdx, rowIdx, a, b, c int) bool {
		for i, sample := range samples {
			meta := metas[i]
			svg := int(meta[svgIdx]) % statsigCurveGroupCount
			row := int(meta[rowIdx]) % statsigCurveRowCount
			product := (int(meta[a]) % 16) * (int(meta[b]) % 16) * (int(meta[c]) % 16)
			if table[svg][row][product] != sample.AnimationKey {
				return false
			}
		}
		return true
	}

	var solutions []StatsigIndexSolution
	for svgIdx := range statsigKeyByteCount {
		for rowIdx := range statsigKeyByteCount {
			for a := range statsigKeyByteCount {
				for b := a; b < statsigKeyByteCount; b++ {
					for c := b; c < statsigKeyByteCount; c++ {
						if matches(svgIdx, rowIdx, a, b, c) {
							solutions = append(solutions, StatsigIndexSolution{svgIdx, rowIdx, a, b, c})
						}
					}
				}
			}
		}
	}
	sort.Slice(solutions, func(i, j int) bool {
		left, right := solutions[i], solutions[j]
		if left.SVG != right.SVG {
			return left.SVG < right.SVG
		}
		if left.Row != right.Row {
			return left.Row < right.Row
		}
		if left.TimeA != right.TimeA {
			return left.TimeA < right.TimeA
		}
		if left.TimeB != right.TimeB {
			return left.TimeB < right.TimeB
		}
		return left.TimeC < right.TimeC
	})
	return solutions, nil
}

// VerifyStatsigByteIndices 用给定下标复算每个样本，返回第一个不吻合的样本序号。
// 全部吻合时返回 -1，便于在换用新下标前做样本外验证。
func VerifyStatsigByteIndices(pageHTML []byte, samples []StatsigCalibrationSample, solution StatsigIndexSolution) (int, error) {
	frames, err := extractStatsigCurves(pageHTML)
	if err != nil {
		return 0, fmt.Errorf("解析 Botox 曲线: %w", err)
	}
	for i, sample := range samples {
		metaBytes, decodeErr := decodeStatsigMetaBytes(sample.Meta)
		if decodeErr != nil {
			return i, decodeErr
		}
		key, keyErr := statsigAnimationKeyWith(metaBytes, frames, solution.indices())
		if keyErr != nil {
			return i, keyErr
		}
		if key != sample.AnimationKey {
			return i, nil
		}
	}
	return -1, nil
}

// ExtractStatsigPageMaterials 复用生产解析路径从页面 HTML 取出 meta 与曲线，
// 让校准与运行时看到的是同一份素材。
func ExtractStatsigPageMaterials(pageHTML []byte) (meta string, curves [][][]int, err error) {
	meta, err = extractStatsigMetaContent(pageHTML)
	if err != nil {
		return "", nil, err
	}
	curves, err = extractStatsigCurves(pageHTML)
	if err != nil {
		return "", nil, err
	}
	return meta, curves, validateStatsigFrames(curves)
}
