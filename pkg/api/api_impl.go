package api

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/evanw/esbuild/internal/bundler"
	"github.com/evanw/esbuild/internal/compat"
	"github.com/evanw/esbuild/internal/config"
	"github.com/evanw/esbuild/internal/fs"
	"github.com/evanw/esbuild/internal/js_ast"
	"github.com/evanw/esbuild/internal/js_lexer"
	"github.com/evanw/esbuild/internal/js_parser"
	"github.com/evanw/esbuild/internal/logger"
	"github.com/evanw/esbuild/internal/resolver"
)

func validatePlatform(value Platform) config.Platform {
	switch value {
	case PlatformBrowser:
		return config.PlatformBrowser
	case PlatformNode:
		return config.PlatformNode
	default:
		panic("Invalid platform")
	}
}

func validateFormat(value Format) config.Format {
	switch value {
	case FormatDefault:
		return config.FormatPreserve
	case FormatIIFE:
		return config.FormatIIFE
	case FormatCommonJS:
		return config.FormatCommonJS
	case FormatESModule:
		return config.FormatESModule
	default:
		panic("Invalid format")
	}
}

func validateSourceMap(value SourceMap) config.SourceMap {
	switch value {
	case SourceMapNone:
		return config.SourceMapNone
	case SourceMapLinked:
		return config.SourceMapLinkedWithComment
	case SourceMapInline:
		return config.SourceMapInline
	case SourceMapExternal:
		return config.SourceMapExternalWithoutComment
	default:
		panic("Invalid source map")
	}
}

func validateColor(value StderrColor) logger.StderrColor {
	switch value {
	case ColorIfTerminal:
		return logger.ColorIfTerminal
	case ColorNever:
		return logger.ColorNever
	case ColorAlways:
		return logger.ColorAlways
	default:
		panic("Invalid color")
	}
}

func validateLogLevel(value LogLevel) logger.LogLevel {
	switch value {
	case LogLevelInfo:
		return logger.LevelInfo
	case LogLevelWarning:
		return logger.LevelWarning
	case LogLevelError:
		return logger.LevelError
	case LogLevelSilent:
		return logger.LevelSilent
	default:
		panic("Invalid log level")
	}
}

func validateASCIIOnly(value Charset) bool {
	switch value {
	case CharsetDefault, CharsetASCII:
		return true
	case CharsetUTF8:
		return false
	default:
		panic("Invalid charset")
	}
}

func validateLoader(value Loader) config.Loader {
	switch value {
	case LoaderNone:
		return config.LoaderNone
	case LoaderJS:
		return config.LoaderJS
	case LoaderJSX:
		return config.LoaderJSX
	case LoaderTS:
		return config.LoaderTS
	case LoaderTSX:
		return config.LoaderTSX
	case LoaderJSON:
		return config.LoaderJSON
	case LoaderText:
		return config.LoaderText
	case LoaderBase64:
		return config.LoaderBase64
	case LoaderDataURL:
		return config.LoaderDataURL
	case LoaderFile:
		return config.LoaderFile
	case LoaderBinary:
		return config.LoaderBinary
	case LoaderCSS:
		return config.LoaderCSS
	case LoaderDefault:
		return config.LoaderDefault
	default:
		panic("Invalid loader")
	}
}

func validateEngine(value EngineName) compat.Engine {
	switch value {
	case EngineChrome:
		return compat.Chrome
	case EngineEdge:
		return compat.Edge
	case EngineFirefox:
		return compat.Firefox
	case EngineIOS:
		return compat.IOS
	case EngineNode:
		return compat.Node
	case EngineSafari:
		return compat.Safari
	default:
		panic("Invalid loader")
	}
}

var versionRegex = regexp.MustCompile(`^([0-9]+)(?:\.([0-9]+))?(?:\.([0-9]+))?$`)

func validateFeatures(log logger.Log, target Target, engines []Engine) (compat.JSFeature, compat.CSSFeature) {
	constraints := make(map[compat.Engine][]int)

	switch target {
	case ES5:
		constraints[compat.ES] = []int{5}
	case ES2015:
		constraints[compat.ES] = []int{2015}
	case ES2016:
		constraints[compat.ES] = []int{2016}
	case ES2017:
		constraints[compat.ES] = []int{2017}
	case ES2018:
		constraints[compat.ES] = []int{2018}
	case ES2019:
		constraints[compat.ES] = []int{2019}
	case ES2020:
		constraints[compat.ES] = []int{2020}
	case ESNext:
	default:
		panic("Invalid target")
	}

	for _, engine := range engines {
		if match := versionRegex.FindStringSubmatch(engine.Version); match != nil {
			if major, err := strconv.Atoi(match[1]); err == nil {
				version := []int{major}
				if minor, err := strconv.Atoi(match[2]); err == nil {
					version = append(version, minor)
				}
				if patch, err := strconv.Atoi(match[3]); err == nil {
					version = append(version, patch)
				}
				switch engine.Name {
				case EngineChrome:
					constraints[compat.Chrome] = version
				case EngineEdge:
					constraints[compat.Edge] = version
				case EngineFirefox:
					constraints[compat.Firefox] = version
				case EngineIOS:
					constraints[compat.IOS] = version
				case EngineNode:
					constraints[compat.Node] = version
				case EngineSafari:
					constraints[compat.Safari] = version
				default:
					panic("Invalid engine name")
				}
				continue
			}
		}

		log.AddError(nil, logger.Loc{}, fmt.Sprintf("Invalid version: %q", engine.Version))
	}

	return compat.UnsupportedJSFeatures(constraints), compat.UnsupportedCSSFeatures(constraints)
}

func validateGlobalName(log logger.Log, text string) []string {
	if text != "" {
		source := logger.Source{
			KeyPath:    logger.Path{Text: "(global path)"},
			PrettyPath: "(global name)",
			Contents:   text,
		}

		if result, ok := js_parser.ParseGlobalName(log, source); ok {
			return result
		}
	}

	return nil
}

func validateExternals(log logger.Log, fs fs.FS, paths []string) config.ExternalModules {
	result := config.ExternalModules{
		NodeModules: make(map[string]bool),
		AbsPaths:    make(map[string]bool),
	}
	for _, path := range paths {
		if resolver.IsPackagePath(path) {
			result.NodeModules[path] = true
		} else if absPath := validatePath(log, fs, path); absPath != "" {
			result.AbsPaths[absPath] = true
		}
	}
	return result
}

func isValidExtension(ext string) bool {
	return len(ext) >= 2 && ext[0] == '.' && ext[len(ext)-1] != '.'
}

func validateResolveExtensions(log logger.Log, order []string) []string {
	if order == nil {
		return []string{".tsx", ".ts", ".jsx", ".mjs", ".cjs", ".js", ".css", ".json"}
	}
	for _, ext := range order {
		if !isValidExtension(ext) {
			log.AddError(nil, logger.Loc{}, fmt.Sprintf("Invalid file extension: %q", ext))
		}
	}
	return order
}

func validateLoaders(log logger.Log, loaders map[string]Loader) map[string]config.Loader {
	result := bundler.DefaultExtensionToLoaderMap()
	if loaders != nil {
		for ext, loader := range loaders {
			if !isValidExtension(ext) {
				log.AddError(nil, logger.Loc{}, fmt.Sprintf("Invalid file extension: %q", ext))
			}
			result[ext] = validateLoader(loader)
		}
	}
	return result
}

func validateJSX(log logger.Log, text string, name string) []string {
	if text == "" {
		return nil
	}
	parts := strings.Split(text, ".")
	for _, part := range parts {
		if !js_lexer.IsIdentifier(part) {
			log.AddError(nil, logger.Loc{}, fmt.Sprintf("Invalid JSX %s: %q", name, text))
			return nil
		}
	}
	return parts
}

func validateDefines(log logger.Log, defines map[string]string, pureFns []string) *config.ProcessedDefines {
	if len(defines) == 0 && len(pureFns) == 0 {
		return nil
	}

	rawDefines := make(map[string]config.DefineData)

	for key, value := range defines {
		// The key must be a dot-separated identifier list
		for _, part := range strings.Split(key, ".") {
			if !js_lexer.IsIdentifier(part) {
				log.AddError(nil, logger.Loc{}, fmt.Sprintf("Invalid define key: %q", key))
				continue
			}
		}

		// Allow substituting for an identifier
		if js_lexer.IsIdentifier(value) {
			if _, ok := js_lexer.Keywords[value]; !ok {
				name := value // The closure must close over a variable inside the loop
				rawDefines[key] = config.DefineData{
					DefineFunc: func(loc logger.Loc, findSymbol config.FindSymbol) js_ast.E {
						return &js_ast.EIdentifier{Ref: findSymbol(loc, name)}
					},
				}
				continue
			}
		}

		// Parse the value as JSON
		source := logger.Source{Contents: value}
		expr, ok := js_parser.ParseJSON(logger.NewDeferLog(), source, js_parser.ParseJSONOptions{})
		if !ok {
			log.AddError(nil, logger.Loc{}, fmt.Sprintf("Invalid define value: %q", value))
			continue
		}

		// Only allow atoms for now
		var fn config.DefineFunc
		switch e := expr.Data.(type) {
		case *js_ast.ENull:
			fn = func(logger.Loc, config.FindSymbol) js_ast.E { return &js_ast.ENull{} }
		case *js_ast.EBoolean:
			fn = func(logger.Loc, config.FindSymbol) js_ast.E { return &js_ast.EBoolean{Value: e.Value} }
		case *js_ast.EString:
			fn = func(logger.Loc, config.FindSymbol) js_ast.E { return &js_ast.EString{Value: e.Value} }
		case *js_ast.ENumber:
			fn = func(logger.Loc, config.FindSymbol) js_ast.E { return &js_ast.ENumber{Value: e.Value} }
		default:
			log.AddError(nil, logger.Loc{}, fmt.Sprintf("Invalid define value: %q", value))
			continue
		}

		rawDefines[key] = config.DefineData{DefineFunc: fn}
	}

	for _, key := range pureFns {
		// The key must be a dot-separated identifier list
		for _, part := range strings.Split(key, ".") {
			if !js_lexer.IsIdentifier(part) {
				log.AddError(nil, logger.Loc{}, fmt.Sprintf("Invalid pure function: %q", key))
				continue
			}
		}

		// Merge with any previously-specified defines
		define := rawDefines[key]
		define.CallCanBeUnwrappedIfUnused = true
		rawDefines[key] = define
	}

	// Processing defines is expensive. Process them once here so the same object
	// can be shared between all parsers we create using these arguments.
	processed := config.ProcessDefines(rawDefines)
	return &processed
}

func validatePath(log logger.Log, fs fs.FS, relPath string) string {
	if relPath == "" {
		return ""
	}
	absPath, ok := fs.Abs(relPath)
	if !ok {
		log.AddError(nil, logger.Loc{}, fmt.Sprintf("Invalid path: %s", relPath))
	}
	return absPath
}

func validateOutputExtensions(log logger.Log, outExtensions map[string]string) map[string]string {
	result := make(map[string]string)
	for key, value := range outExtensions {
		if key != ".js" && key != ".css" {
			log.AddError(nil, logger.Loc{}, fmt.Sprintf("Invalid output extension: %q (valid: .css, .js)", key))
		}
		if !isValidExtension(value) {
			log.AddError(nil, logger.Loc{}, fmt.Sprintf("Invalid output extension: %q", value))
		}
		result[key] = value
	}
	return result
}

func convertMessagesToPublic(kind logger.MsgKind, msgs []logger.Msg) []Message {
	var filtered []Message
	for _, msg := range msgs {
		if msg.Kind == kind {
			var location *Location
			loc := msg.Location

			if loc != nil {
				location = &Location{
					File:      loc.File,
					Namespace: loc.Namespace,
					Line:      loc.Line,
					Column:    loc.Column,
					Length:    loc.Length,
					LineText:  loc.LineText,
				}
			}

			filtered = append(filtered, Message{
				Text:     msg.Text,
				Location: location,
			})
		}
	}
	return filtered
}

func convertMessagesToInternal(msgs []logger.Msg, kind logger.MsgKind, messages []Message) []logger.Msg {
	for _, message := range messages {
		var location *logger.MsgLocation
		loc := message.Location

		if loc != nil {
			namespace := loc.Namespace
			if namespace == "" {
				namespace = "file"
			}
			location = &logger.MsgLocation{
				File:      loc.File,
				Namespace: namespace,
				Line:      loc.Line,
				Column:    loc.Column,
				Length:    loc.Length,
				LineText:  loc.LineText,
			}
		}

		msgs = append(msgs, logger.Msg{
			Kind:     kind,
			Text:     message.Text,
			Location: location,
		})
	}
	return msgs
}

////////////////////////////////////////////////////////////////////////////////
// Build API

func buildImpl(buildOpts BuildOptions) BuildResult {
	log := logger.NewStderrLog(logger.StderrOptions{
		IncludeSource: true,
		ErrorLimit:    buildOpts.ErrorLimit,
		Color:         validateColor(buildOpts.Color),
		LogLevel:      validateLogLevel(buildOpts.LogLevel),
	})

	// Convert and validate the buildOpts
	realFS := fs.RealFS()
	jsFeatures, cssFeatures := validateFeatures(log, buildOpts.Target, buildOpts.Engines)
	options := config.Options{
		UnsupportedJSFeatures:  jsFeatures,
		UnsupportedCSSFeatures: cssFeatures,
		JSX: config.JSXOptions{
			Factory:  validateJSX(log, buildOpts.JSXFactory, "factory"),
			Fragment: validateJSX(log, buildOpts.JSXFragment, "fragment"),
		},
		Defines:           validateDefines(log, buildOpts.Define, buildOpts.Pure),
		Platform:          validatePlatform(buildOpts.Platform),
		SourceMap:         validateSourceMap(buildOpts.Sourcemap),
		MangleSyntax:      buildOpts.MinifySyntax,
		RemoveWhitespace:  buildOpts.MinifyWhitespace,
		MinifyIdentifiers: buildOpts.MinifyIdentifiers,
		ASCIIOnly:         validateASCIIOnly(buildOpts.Charset),
		ModuleName:        validateGlobalName(log, buildOpts.GlobalName),
		CodeSplitting:     buildOpts.Splitting,
		OutputFormat:      validateFormat(buildOpts.Format),
		AbsOutputFile:     validatePath(log, realFS, buildOpts.Outfile),
		AbsOutputDir:      validatePath(log, realFS, buildOpts.Outdir),
		AbsOutputBase:     validatePath(log, realFS, buildOpts.Outbase),
		AbsMetadataFile:   validatePath(log, realFS, buildOpts.Metafile),
		OutputExtensions:  validateOutputExtensions(log, buildOpts.OutExtensions),
		ExtensionToLoader: validateLoaders(log, buildOpts.Loader),
		ExtensionOrder:    validateResolveExtensions(log, buildOpts.ResolveExtensions),
		ExternalModules:   validateExternals(log, realFS, buildOpts.External),
		TsConfigOverride:  validatePath(log, realFS, buildOpts.Tsconfig),
		MainFields:        buildOpts.MainFields,
		PublicPath:        buildOpts.PublicPath,
		AvoidTDZ:          buildOpts.AvoidTDZ,
		KeepNames:         buildOpts.KeepNames,
		InjectAbsPaths:    make([]string, len(buildOpts.Inject)),
	}
	for i, path := range buildOpts.Inject {
		options.InjectAbsPaths[i] = validatePath(log, realFS, path)
	}
	if options.PublicPath != "" && !strings.HasSuffix(options.PublicPath, "/") && !strings.HasSuffix(options.PublicPath, "\\") {
		options.PublicPath += "/"
	}
	entryPaths := make([]string, len(buildOpts.EntryPoints))
	for i, entryPoint := range buildOpts.EntryPoints {
		entryPaths[i] = validatePath(log, realFS, entryPoint)
	}
	entryPathCount := len(buildOpts.EntryPoints)
	if buildOpts.Stdin != nil {
		entryPathCount++
		options.Stdin = &config.StdinInfo{
			Loader:        validateLoader(buildOpts.Stdin.Loader),
			Contents:      buildOpts.Stdin.Contents,
			SourceFile:    buildOpts.Stdin.Sourcefile,
			AbsResolveDir: validatePath(log, realFS, buildOpts.Stdin.ResolveDir),
		}
	}

	if options.AbsOutputDir == "" && entryPathCount > 1 {
		log.AddError(nil, logger.Loc{},
			"Must use \"outdir\" when there are multiple input files")
	} else if options.AbsOutputDir == "" && options.CodeSplitting {
		log.AddError(nil, logger.Loc{},
			"Must use \"outdir\" when code splitting is enabled")
	} else if options.AbsOutputFile != "" && options.AbsOutputDir != "" {
		log.AddError(nil, logger.Loc{}, "Cannot use both \"outfile\" and \"outdir\"")
	} else if options.AbsOutputFile != "" {
		// If the output file is specified, use it to derive the output directory
		options.AbsOutputDir = realFS.Dir(options.AbsOutputFile)
	} else if options.AbsOutputDir == "" {
		options.WriteToStdout = true

		// Forbid certain features when writing to stdout
		if options.SourceMap != config.SourceMapNone && options.SourceMap != config.SourceMapInline {
			log.AddError(nil, logger.Loc{}, "Cannot use an external source map without an output path")
		}
		if options.AbsMetadataFile != "" {
			log.AddError(nil, logger.Loc{}, "Cannot use \"metafile\" without an output path")
		}
		for _, loader := range options.ExtensionToLoader {
			if loader == config.LoaderFile {
				log.AddError(nil, logger.Loc{}, "Cannot use the \"file\" loader without an output path")
				break
			}
		}

		// Use the current directory as the output directory instead of an empty
		// string because external modules with relative paths need a base directory.
		options.AbsOutputDir = realFS.Cwd()
	}

	if !buildOpts.Bundle {
		// Disallow bundle-only options when not bundling
		if len(options.ExternalModules.NodeModules) > 0 || len(options.ExternalModules.AbsPaths) > 0 {
			log.AddError(nil, logger.Loc{}, "Cannot use \"external\" without \"bundle\"")
		}
	} else if options.OutputFormat == config.FormatPreserve {
		// If the format isn't specified, set the default format using the platform
		switch options.Platform {
		case config.PlatformBrowser:
			options.OutputFormat = config.FormatIIFE
		case config.PlatformNode:
			options.OutputFormat = config.FormatCommonJS
		}
	}

	// Set the output mode using other settings
	if buildOpts.Bundle {
		options.Mode = config.ModeBundle
	} else if options.OutputFormat != config.FormatPreserve {
		options.Mode = config.ModeConvertFormat
	}

	// Code splitting is experimental and currently only enabled for ES6 modules
	if options.CodeSplitting && options.OutputFormat != config.FormatESModule {
		log.AddError(nil, logger.Loc{}, "Splitting currently only works with the \"esm\" format")
	}

	loadPlugins(&options, realFS, log, buildOpts.Plugins)

	var outputFiles []OutputFile

	// Stop now if there were errors
	if !log.HasErrors() {
		// Scan over the bundle
		resolver := resolver.NewResolver(realFS, log, options)
		bundle := bundler.ScanBundle(log, realFS, resolver, entryPaths, options)

		// Stop now if there were errors
		if !log.HasErrors() {
			// Compile the bundle
			results := bundle.Compile(log, options)

			// Stop now if there were errors
			if !log.HasErrors() {
				if buildOpts.Write {
					// Special-case writing to stdout
					if options.WriteToStdout {
						if len(results) != 1 {
							log.AddError(nil, logger.Loc{}, fmt.Sprintf(
								"Internal error: did not expect to generate %d files when writing to stdout", len(results)))
						} else if _, err := os.Stdout.Write(results[0].Contents); err != nil {
							log.AddError(nil, logger.Loc{}, fmt.Sprintf(
								"Failed to write to stdout: %s", err.Error()))
						}
					} else {
						// Write out files in parallel
						waitGroup := sync.WaitGroup{}
						waitGroup.Add(len(results))
						for _, result := range results {
							go func(result bundler.OutputFile) {
								fs.BeforeFileOpen()
								defer fs.AfterFileClose()
								if err := os.MkdirAll(filepath.Dir(result.AbsPath), 0755); err != nil {
									log.AddError(nil, logger.Loc{}, fmt.Sprintf(
										"Failed to create output directory: %s", err.Error()))
								} else {
									var mode os.FileMode = 0644
									if result.IsExecutable {
										mode = 0755
									}
									if err := ioutil.WriteFile(result.AbsPath, result.Contents, mode); err != nil {
										log.AddError(nil, logger.Loc{}, fmt.Sprintf(
											"Failed to write to output file: %s", err.Error()))
									}
								}
								waitGroup.Done()
							}(result)
						}
						waitGroup.Wait()
					}
				}

				// Return the results
				outputFiles = make([]OutputFile, len(results))
				for i, result := range results {
					if options.WriteToStdout {
						result.AbsPath = "<stdout>"
					}
					outputFiles[i] = OutputFile{
						Path:     result.AbsPath,
						Contents: result.Contents,
					}
				}
			}
		}
	}

	msgs := log.Done()
	return BuildResult{
		Errors:      convertMessagesToPublic(logger.Error, msgs),
		Warnings:    convertMessagesToPublic(logger.Warning, msgs),
		OutputFiles: outputFiles,
	}
}

////////////////////////////////////////////////////////////////////////////////
// Transform API

func transformImpl(input string, transformOpts TransformOptions) TransformResult {
	log := logger.NewStderrLog(logger.StderrOptions{
		IncludeSource: true,
		ErrorLimit:    transformOpts.ErrorLimit,
		Color:         validateColor(transformOpts.Color),
		LogLevel:      validateLogLevel(transformOpts.LogLevel),
	})

	// Settings from the user come first
	preserveUnusedImportsTS := false
	useDefineForClassFieldsTS := false
	jsx := config.JSXOptions{
		Factory:  validateJSX(log, transformOpts.JSXFactory, "factory"),
		Fragment: validateJSX(log, transformOpts.JSXFragment, "fragment"),
	}

	// Settings from "tsconfig.json" override those
	if transformOpts.TsconfigRaw != "" {
		source := logger.Source{
			KeyPath:    logger.Path{Text: "tsconfig.json"},
			PrettyPath: "tsconfig.json",
			Contents:   transformOpts.TsconfigRaw,
		}
		if result := resolver.ParseTSConfigJSON(log, source, nil); result != nil {
			if len(result.JSXFactory) > 0 {
				jsx.Factory = result.JSXFactory
			}
			if len(result.JSXFragmentFactory) > 0 {
				jsx.Fragment = result.JSXFragmentFactory
			}
			if result.UseDefineForClassFields {
				useDefineForClassFieldsTS = true
			}
			if result.PreserveImportsNotUsedAsValues {
				preserveUnusedImportsTS = true
			}
		}
	}

	// Apply default values
	if transformOpts.Sourcefile == "" {
		transformOpts.Sourcefile = "<stdin>"
	}
	if transformOpts.Loader == LoaderNone {
		transformOpts.Loader = LoaderJS
	}

	// Convert and validate the transformOpts
	jsFeatures, cssFeatures := validateFeatures(log, transformOpts.Target, transformOpts.Engines)
	options := config.Options{
		UnsupportedJSFeatures:   jsFeatures,
		UnsupportedCSSFeatures:  cssFeatures,
		JSX:                     jsx,
		Defines:                 validateDefines(log, transformOpts.Define, transformOpts.Pure),
		SourceMap:               validateSourceMap(transformOpts.Sourcemap),
		OutputFormat:            validateFormat(transformOpts.Format),
		ModuleName:              validateGlobalName(log, transformOpts.GlobalName),
		MangleSyntax:            transformOpts.MinifySyntax,
		RemoveWhitespace:        transformOpts.MinifyWhitespace,
		MinifyIdentifiers:       transformOpts.MinifyIdentifiers,
		ASCIIOnly:               validateASCIIOnly(transformOpts.Charset),
		AbsOutputFile:           transformOpts.Sourcefile + "-out",
		AvoidTDZ:                transformOpts.AvoidTDZ,
		KeepNames:               transformOpts.KeepNames,
		UseDefineForClassFields: useDefineForClassFieldsTS,
		PreserveUnusedImportsTS: preserveUnusedImportsTS,
		Stdin: &config.StdinInfo{
			Loader:     validateLoader(transformOpts.Loader),
			Contents:   input,
			SourceFile: transformOpts.Sourcefile,
		},
	}
	if options.SourceMap == config.SourceMapLinkedWithComment {
		// Linked source maps don't make sense because there's no output file name
		log.AddError(nil, logger.Loc{}, "Cannot transform with linked source maps")
	}
	if options.SourceMap != config.SourceMapNone && options.Stdin.SourceFile == "" {
		log.AddError(nil, logger.Loc{},
			"Must use \"sourcefile\" with \"sourcemap\" to set the original file name")
	}

	// Set the output mode using other settings
	if options.OutputFormat != config.FormatPreserve {
		options.Mode = config.ModeConvertFormat
	}

	var results []bundler.OutputFile

	// Stop now if there were errors
	if !log.HasErrors() {
		// Scan over the bundle
		mockFS := fs.MockFS(make(map[string]string))
		resolver := resolver.NewResolver(mockFS, log, options)
		bundle := bundler.ScanBundle(log, mockFS, resolver, nil, options)

		// Stop now if there were errors
		if !log.HasErrors() {
			// Compile the bundle
			results = bundle.Compile(log, options)
		}
	}

	// Return the results
	var code []byte
	var sourceMap []byte

	// Unpack the JavaScript file and the source map file
	if len(results) == 1 {
		code = results[0].Contents
	} else if len(results) == 2 {
		a, b := results[0], results[1]
		if a.AbsPath == b.AbsPath+".map" {
			sourceMap, code = a.Contents, b.Contents
		} else if a.AbsPath+".map" == b.AbsPath {
			code, sourceMap = a.Contents, b.Contents
		}
	}

	msgs := log.Done()
	return TransformResult{
		Errors:   convertMessagesToPublic(logger.Error, msgs),
		Warnings: convertMessagesToPublic(logger.Warning, msgs),
		Code:     code,
		Map:      sourceMap,
	}
}

////////////////////////////////////////////////////////////////////////////////
// Plugin API

type pluginImpl struct {
	log    logger.Log
	fs     fs.FS
	plugin config.Plugin
}

func (impl *pluginImpl) OnResolve(options OnResolveOptions, callback func(OnResolveArgs) (OnResolveResult, error)) {
	filter, err := config.CompileFilterForPlugin(impl.plugin.Name, "OnResolve", options.Filter)
	if filter == nil {
		impl.log.AddError(nil, logger.Loc{}, err.Error())
		return
	}

	impl.plugin.OnResolve = append(impl.plugin.OnResolve, config.OnResolve{
		Name:      impl.plugin.Name,
		Filter:    filter,
		Namespace: options.Namespace,
		Callback: func(args config.OnResolveArgs) (result config.OnResolveResult) {
			response, err := callback(OnResolveArgs{
				Path:       args.Path,
				Importer:   args.Importer.Text,
				Namespace:  args.Importer.Namespace,
				ResolveDir: args.ResolveDir,
			})
			result.PluginName = response.PluginName

			if err != nil {
				result.ThrownError = err
				return
			}

			result.Path = logger.Path{Text: response.Path, Namespace: response.Namespace}
			result.External = response.External

			// Convert log messages
			if len(response.Errors)+len(response.Warnings) > 0 {
				msgs := make(sortableMsgs, 0, len(response.Errors)+len(response.Warnings))
				msgs = convertMessagesToInternal(msgs, logger.Error, response.Errors)
				msgs = convertMessagesToInternal(msgs, logger.Warning, response.Warnings)
				sort.Sort(msgs)
				result.Msgs = msgs
			}
			return
		},
	})
}

func (impl *pluginImpl) OnLoad(options OnLoadOptions, callback func(OnLoadArgs) (OnLoadResult, error)) {
	filter, err := config.CompileFilterForPlugin(impl.plugin.Name, "OnLoad", options.Filter)
	if filter == nil {
		impl.log.AddError(nil, logger.Loc{}, err.Error())
		return
	}

	impl.plugin.OnLoad = append(impl.plugin.OnLoad, config.OnLoad{
		Filter:    filter,
		Namespace: options.Namespace,
		Callback: func(args config.OnLoadArgs) (result config.OnLoadResult) {
			response, err := callback(OnLoadArgs{
				Path:      args.Path.Text,
				Namespace: args.Path.Namespace,
			})
			result.PluginName = response.PluginName

			if err != nil {
				result.ThrownError = err
				return
			}

			result.Contents = response.Contents
			result.Loader = validateLoader(response.Loader)
			if absPath := validatePath(impl.log, impl.fs, response.ResolveDir); absPath != "" {
				result.AbsResolveDir = absPath
			}

			// Convert log messages
			if len(response.Errors)+len(response.Warnings) > 0 {
				msgs := make(sortableMsgs, 0, len(response.Errors)+len(response.Warnings))
				msgs = convertMessagesToInternal(msgs, logger.Error, response.Errors)
				msgs = convertMessagesToInternal(msgs, logger.Warning, response.Warnings)
				sort.Sort(msgs)
				result.Msgs = msgs
			}
			return
		},
	})
}

// This type is just so we can use Go's native sort function
type sortableMsgs []logger.Msg

func (a sortableMsgs) Len() int          { return len(a) }
func (a sortableMsgs) Swap(i int, j int) { a[i], a[j] = a[j], a[i] }

func (a sortableMsgs) Less(i int, j int) bool {
	ai := a[i].Location
	aj := a[j].Location
	if ai == nil || aj == nil {
		return ai == nil && aj != nil
	}
	if ai.File != aj.File {
		return ai.File < aj.File
	}
	if ai.Line != aj.Line {
		return ai.Line < aj.Line
	}
	if ai.Column != aj.Column {
		return ai.Column < aj.Column
	}
	return a[i].Text < a[j].Text
}

func loadPlugins(options *config.Options, fs fs.FS, log logger.Log, plugins []Plugin) {
	for i, item := range plugins {
		if item.Name == "" {
			log.AddError(nil, logger.Loc{}, fmt.Sprintf("Plugin at index %d is missing a name", i))
			continue
		}

		impl := &pluginImpl{
			fs:     fs,
			log:    log,
			plugin: config.Plugin{Name: item.Name},
		}

		item.Setup(impl)
		options.Plugins = append(options.Plugins, impl.plugin)
	}
}
