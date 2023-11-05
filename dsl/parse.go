package dsl

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gosimple/slug"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/rs/zerolog"
	"github.com/zclconf/go-cty/cty/gocty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

const hopsMetadataKey = "hops"

// Array of parsed .hops files
type HclFiles []*hcl.File

// can be in a list of filenames and content
// needed for parsing
type fileContent struct {
	file    string
	content []byte
}

// ReadHopsFiles loads and pre-parses the content of .hops files either from a
// single file or from all .hops files in a directory.
// It returns a reference to the parsed files `HclFiles` and a sha hash of the contents
func ReadHopsFiles(filePath string) (hcl.Body, string, error) {
	var files []fileContent

	info, err := os.Stat(filePath)
	if err != nil {
		return nil, "", err
	}

	// read in the hops files and prepare for parsing
	if info.IsDir() {
		files, err = readHopsDir(filePath)
		if err != nil {
			return nil, "", err
		}
	} else {
		content, err := os.ReadFile(filePath)
		if err != nil {
			return nil, "", err
		}

		files = []fileContent{{
			file:    filePath,
			content: content,
		}}
	}

	hopsBodies := []hcl.Body{}
	parser := hclparse.NewParser()
	sha1Hash := sha1.New()

	// parse the hops files
	for _, file := range files {
		hopsFile, diags := parser.ParseHCL(file.content, file.file)

		if diags != nil && diags.HasErrors() {
			return nil, "", errors.New(diags.Error())
		}
		hopsBodies = append(hopsBodies, hopsFile.Body)

		sha1Hash.Write(file.content)
	}

	filesSha := sha1Hash.Sum(nil)
	filesShaHex := hex.EncodeToString(filesSha)

	body := hcl.MergeBodies(hopsBodies)

	return body, filesShaHex, nil
}

// readHopsDir retrieves the content of all .hops files in a directory,
// including sub directories, and returns then as a slice of fileContents
func readHopsDir(dirPath string) ([]fileContent, error) {
	var filePaths []string

	err := filepath.WalkDir(dirPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Exclude directories whose name starts with '..'
		// This is because kubernetes configMaps create a set of symlinked
		// directories for the mapped files and we don't want to pick those
		// up. Those directories are named '..<various names>'
		// Example:
		// /my-config-map-dir
		// |-- my-key -> ..data/my-key
		// |-- ..data -> ..2023_10_19_12_34_56.789012345
		// |-- ..2023_10_19_12_34_56.789012345
		// |   |-- my-key
		if d.IsDir() && strings.HasPrefix(d.Name(), "..") {
			return filepath.SkipDir
		}
		if !d.IsDir() && filepath.Ext(path) == ".hops" {
			filePaths = append(filePaths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Sort the file paths to ensure consistent order
	sort.Strings(filePaths)

	files := []fileContent{}

	// Read and store filename and content of each file
	for _, filePath := range filePaths {
		content, err := os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}
		files = append(files, fileContent{
			file:    filePath,
			content: content,
		})
	}

	return files, nil
}

func ParseHops(ctx context.Context, hopsBody hcl.Body, eventBundle map[string][]byte, logger zerolog.Logger) (*HopAST, error) {
	hop := &HopAST{
		SlugRegister: make(map[string]bool),
	}

	ctxVariables, err := eventBundleToCty(eventBundle, "-")
	if err != nil {
		return nil, err
	}

	evalctx := &hcl.EvalContext{
		Functions: DefaultFunctions,
		Variables: ctxVariables,
	}

	err = DecodeHopsBody(ctx, hop, hopsBody, evalctx, logger)
	if err != nil {
		logger.Error().Err(err).Msg("Failed to decode hops file")
		logger.Debug().RawJSON("source_event", eventBundle["event"]).Msg("Parse failed on source event")
		return hop, err
	}

	return hop, nil
}

func DecodeHopsBody(ctx context.Context, hop *HopAST, body hcl.Body, evalctx *hcl.EvalContext, logger zerolog.Logger) error {
	bc, d := body.Content(HopSchema)
	if d.HasErrors() {
		return d.Errs()[0]
	}

	if len(bc.Blocks) == 0 {
		return errors.New("At least one resource must be defined")
	}

	onBlocks := bc.Blocks.OfType(OnID)
	for idx, onBlock := range onBlocks {
		err := DecodeOnBlock(ctx, hop, onBlock, idx, evalctx, logger)
		if err != nil {
			return err
		}
	}

	return nil
}

func DecodeOnBlock(ctx context.Context, hop *HopAST, block *hcl.Block, idx int, evalctx *hcl.EvalContext, logger zerolog.Logger) error {
	on := &OnAST{}

	bc, d := block.Body.Content(OnSchema)
	if d.HasErrors() {
		return errors.New(d.Error())
	}

	on.EventType = block.Labels[0]
	name, err := DecodeNameAttr(bc.Attributes[NameAttr])
	if err != nil {
		return err
	}
	// If no name is given, append stringified index of the block
	if name == "" {
		name = fmt.Sprintf("%s%d", on.EventType, idx)
	}

	on.Name = name
	on.Slug = slugify(on.Name)

	err = ValidateLabels(on.EventType, on.Name)
	if err != nil {
		return err
	}

	if hop.SlugRegister[on.Slug] {
		return fmt.Errorf("Duplicate 'on' block found: %s", on.Slug)
	} else {
		hop.SlugRegister[on.Slug] = true
	}

	// TODO: This should be done once outside of the on block and passed in as an argument
	eventType, eventAction, err := parseEventVar(evalctx.Variables)
	if err != nil {
		return err
	}

	blockEventType, blockAction, hasAction := strings.Cut(on.EventType, "_")
	if blockEventType != eventType {
		logger.Debug().Msgf("%s does not match event type %s", on.Slug, eventType)
		return nil
	}
	if hasAction && blockAction != eventAction {
		logger.Debug().Msgf("%s does not match event action %s", on.Slug, eventAction)
		return nil
	}

	evalctx = scopedEvalContext(evalctx, on.EventType, on.Name)

	ifClause := bc.Attributes[IfAttr]
	if ifClause != nil {
		val, err := DecodeConditionalAttr(ifClause, evalctx)
		if err != nil {
			return err
		}

		// If condition is not met. Omit the block and stop parsing.
		if !val {
			logger.Debug().Msgf("%s 'if' not met", on.Slug)
			return nil
		}

		on.IfClause = val
	} else {
		on.IfClause = true
	}

	logger.Info().Msgf("%s matches event", on.Slug)

	callBlocks := bc.Blocks.OfType(CallID)
	for idx, callBlock := range callBlocks {
		err := DecodeCallBlock(ctx, hop, on, callBlock, idx, evalctx, logger)
		if err != nil {
			return err
		}
	}

	hop.Ons = append(hop.Ons, *on)
	return nil
}

func DecodeCallBlock(ctx context.Context, hop *HopAST, on *OnAST, block *hcl.Block, idx int, evalctx *hcl.EvalContext, logger zerolog.Logger) error {
	call := &CallAST{}

	bc, d := block.Body.Content(callSchema)
	if d.HasErrors() {
		return errors.New(d.Error())
	}

	call.TaskType = block.Labels[0]
	name, err := DecodeNameAttr(bc.Attributes[NameAttr])
	if err != nil {
		return err
	}
	if name == "" {
		name = fmt.Sprintf("%s%d", call.TaskType, idx)
	}

	call.Name = name
	call.Slug = slugify(on.Slug, call.Name)

	err = ValidateLabels(call.TaskType, call.Name)
	if err != nil {
		return err
	}

	if hop.SlugRegister[call.Slug] {
		return fmt.Errorf("Duplicate call block found: %s", call.Slug)
	} else {
		hop.SlugRegister[call.Slug] = true
	}

	ifClause := bc.Attributes[IfAttr]
	if ifClause != nil {
		val, err := DecodeConditionalAttr(ifClause, evalctx)
		if err != nil {
			logger.Debug().Msgf(
				"%s 'if' not ready for evaluation, defaulting to false: %s",
				call.Slug,
				err.Error(),
			)
		}

		if !val {
			logger.Debug().Msgf("%s 'if' not met", call.Slug)
			return nil
		}

		call.IfClause = val
	} else {
		call.IfClause = true
	}

	logger.Info().Msgf("%s matches event", call.Slug)

	inputs := bc.Attributes["inputs"]
	if inputs != nil {
		val, d := inputs.Expr.Value(evalctx)
		if d.HasErrors() {
			return errors.New(d.Error())
		}

		jsonVal := ctyjson.SimpleJSONValue{Value: val}
		inputs, err := jsonVal.MarshalJSON()

		if err != nil {
			return err
		}

		call.Inputs = inputs
	}

	on.Calls = append(on.Calls, *call)
	return nil
}

func DecodeNameAttr(attr *hcl.Attribute) (string, error) {
	if attr == nil {
		// Not an error, as the attribute is not required
		return "", nil
	}

	val, diag := attr.Expr.Value(nil)
	if diag.HasErrors() {
		return "", errors.New(diag.Error())
	}

	var value string

	err := gocty.FromCtyValue(val, &value)
	if err != nil {
		return "", fmt.Errorf("%s %w", attr.NameRange, err)
	}

	return value, nil
}

func DecodeConditionalAttr(attr *hcl.Attribute, ctx *hcl.EvalContext) (bool, error) {
	if attr == nil {
		return true, nil
	}

	v, diag := attr.Expr.Value(ctx)
	if diag.HasErrors() {
		return false, errors.New(diag.Error())
	}

	var value bool

	err := gocty.FromCtyValue(v, &value)
	if err != nil {
		return false, fmt.Errorf("%s %w", attr.NameRange, err)
	}

	return value, nil
}

func slugify(parts ...string) string {
	joined := strings.Join(parts, "-")
	return slug.Make(joined)
}

// scopedEvalContext creates eval contexts that are relative to the current scope
//
// This function effectively fakes relative/local variables by checking where
// we are in the hops code (defined by scopePath) and bringing any nested variables matching
// that path to the top level.
func scopedEvalContext(evalCtx *hcl.EvalContext, scopePath ...string) *hcl.EvalContext {
	scopedVars := evalCtx.Variables

	for _, scopeToken := range scopePath {
		if val, ok := scopedVars[scopeToken]; ok {
			scopedVars = val.AsValueMap()
		}
	}

	scopedEvalCtx := evalCtx.NewChild()
	scopedEvalCtx.Variables = scopedVars

	return scopedEvalCtx
}
