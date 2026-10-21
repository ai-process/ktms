package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// ComputeKey returns a deterministic hash key for an exact-match request.
// The key includes the calling client's APIKeyID (tenant isolation), the configuration
// snapshot version (invalidation on rule changes), and canonical request parameters.
func ComputeKey(apiKeyID string, configVersion int64, req *pb.GenerateTextRequest) string {
	h := sha256.New()

	// Tenant isolation and routing configuration version
	fmt.Fprintf(h, "key_id:%s\n", apiKeyID)
	fmt.Fprintf(h, "cfg_ver:%d\n", configVersion)

	if req == nil {
		return hex.EncodeToString(h.Sum(nil))
	}
	writeParams(h, req, true)
	for i, m := range req.GetMessages() {
		writeMessage(h, i, m)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeParams hashes everything but the messages, in the order ComputeKey has
// always used so existing cache keys stay valid.
func writeParams(h io.Writer, req *pb.GenerateTextRequest, withRequestInstruction bool) {
	fmt.Fprintf(h, "effort:%s\n", req.GetEffort().String())
	fmt.Fprintf(h, "override:%s\n", req.GetModelOverride())
	fmt.Fprintf(h, "base_sys:%s\n", req.GetBaseSystemInstruction())
	if withRequestInstruction {
		fmt.Fprintf(h, "req_inst:%s\n", req.GetRequestInstruction())
	}
	fmt.Fprintf(h, "max_tokens:%d\n", req.GetMaxOutputTokens())
	fmt.Fprintf(h, "search:%t\n", req.GetEnableGoogleSearch())

	// Sorted attributes
	if attrs := req.GetAttributes(); len(attrs) > 0 {
		keys := make([]string, 0, len(attrs))
		for k := range attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(h, "attr:%s=%s\n", k, attrs[k])
		}
	}

	// Response schema canonical representation
	if schema := req.GetResponseSchema(); schema != nil {
		writeResponseSchema(h, schema)
	}

	// Tool choice
	if tc := req.GetToolChoice(); tc != nil {
		fmt.Fprintf(h, "tool_choice:%s:%s\n", tc.GetMode().String(), tc.GetSpecificFunctionName())
	}

	// Tools
	if tools := req.GetTools(); len(tools) > 0 {
		for i, t := range tools {
			fmt.Fprintf(h, "tool:%d:%s:%s:%s:%t\n", i, t.GetType(), t.GetFunction().GetName(), t.GetFunction().GetDescription(), t.GetFunction().GetStrict())
			if params := t.GetFunction().GetParameters(); params != nil {
				writeResponseSchema(h, params)
			}
		}
	}
}

func writeMessage(h io.Writer, i int, m *pb.ChatMessage) {
	fmt.Fprintf(h, "msg:%d:%s:%s:%s\n", i, m.GetRole().String(), m.GetText(), m.GetToolCallId())
	for j, tc := range m.GetToolCalls() {
		fmt.Fprintf(h, "tc:%d:%d:%s:%s:%s:%s\n", i, j, tc.GetId(), tc.GetType(), tc.GetFunction().GetName(), tc.GetFunction().GetArguments())
	}
	for k, p := range m.GetParts() {
		switch part := p.GetPart().(type) {
		case *pb.ContentPart_Text:
			fmt.Fprintf(h, "part_txt:%d:%d:%s\n", i, k, part.Text.GetText())
		case *pb.ContentPart_Image:
			img := part.Image
			fmt.Fprintf(h, "part_img:%d:%d:%s:%s:%s:%d\n", i, k, img.GetMimeType(), img.GetUrl(), img.GetDetail(), len(img.GetData()))
			if len(img.GetData()) > 0 {
				dataHash := sha256.Sum256(img.GetData())
				fmt.Fprintf(h, "part_img_hash:%s\n", hex.EncodeToString(dataHash[:]))
			}
		case *pb.ContentPart_Audio:
			aud := part.Audio
			fmt.Fprintf(h, "part_aud:%d:%d:%s:%s:%d\n", i, k, aud.GetMimeType(), aud.GetFormat(), len(aud.GetData()))
			if len(aud.GetData()) > 0 {
				dataHash := sha256.Sum256(aud.GetData())
				fmt.Fprintf(h, "part_aud_hash:%s\n", hex.EncodeToString(dataHash[:]))
			}
		case *pb.ContentPart_Document:
			doc := part.Document
			fmt.Fprintf(h, "part_doc:%d:%d:%s:%s:%d\n", i, k, doc.GetMimeType(), doc.GetFileName(), len(doc.GetData()))
			if len(doc.GetData()) > 0 {
				dataHash := sha256.Sum256(doc.GetData())
				fmt.Fprintf(h, "part_doc_hash:%s\n", hex.EncodeToString(dataHash[:]))
			}
		}
	}
}

func writeResponseSchema(w io.Writer, schema *pb.ResponseSchema) {
	fmt.Fprintf(w, "schema:%s:%s\n", schema.GetType(), optionalBoolKey(schema.AdditionalProperties))
	for _, req := range schema.GetRequired() {
		fmt.Fprintf(w, "schema_req:%s\n", req)
	}
	if props := schema.GetProperties(); len(props) > 0 {
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			writeSchemaProperty(w, k, props[k])
		}
	}
	if items := schema.GetItems(); items != nil {
		writeSchemaProperty(w, "items", items)
	}
}

func writeSchemaProperty(w io.Writer, name string, prop *pb.SchemaProperty) {
	if prop == nil {
		return
	}
	fmt.Fprintf(w, "prop:%s:%s:%s:%s\n", name, prop.GetType(), prop.GetDescription(), strconv.FormatBool(prop.GetNullable()))
	// Schemas that differ only in these constrain the reply differently, so
	// they must not share a cached answer.
	fmt.Fprintf(w, "prop_fmt:%s:%q:%s\n", name, prop.GetFormat(), optionalBoolKey(prop.AdditionalProperties))
	for _, v := range prop.GetEnum() {
		fmt.Fprintf(w, "prop_enum:%s:%q\n", name, v)
	}
	for _, req := range prop.GetRequired() {
		fmt.Fprintf(w, "prop_req:%s:%s\n", name, req)
	}
	if props := prop.GetProperties(); len(props) > 0 {
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			writeSchemaProperty(w, name+"."+k, props[k])
		}
	}
	if items := prop.GetItems(); items != nil {
		writeSchemaProperty(w, name+".items", items)
	}
}

func optionalBoolKey(b *bool) string {
	if b == nil {
		return "-"
	}
	return strconv.FormatBool(*b)
}
