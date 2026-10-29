package jsonschema

// Review is the JSON Schema of output.ReviewOutput passed to Claude.
const Review = `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "additionalProperties": false,
  "required": ["summary", "walkthrough", "findings", "resolved"],
  "properties": {
    "summary": {
      "type": "string",
      "description": "Short overall assessment of the changes (2-6 sentences)."
    },
    "diagram": {
      "type": "string",
      "description": "Optional Mermaid diagram (sequenceDiagram or flowchart) of what the merge request changes, without code fences; empty string when a diagram adds nothing."
    },
    "walkthrough": {
      "type": "array",
      "description": "One entry per changed file in the reviewed range.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["path", "summary"],
        "properties": {
          "path": {"type": "string"},
          "summary": {"type": "string", "description": "One line: what changed in this file."}
        }
      }
    },
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["path", "line", "end_line", "severity", "title", "body"],
        "properties": {
          "path": {"type": "string", "description": "File path relative to the repository root (new path)."},
          "line": {"type": "integer", "minimum": 0, "description": "First line in the NEW file; 0 if the remark is about the file as a whole."},
          "end_line": {"type": "integer", "minimum": 0, "description": "Last line in the NEW file covered by the remark (>= line)."},
          "severity": {"type": "string", "enum": ["critical", "major", "minor", "nit"]},
          "title": {"type": "string", "description": "One-line summary of the problem."},
          "body": {"type": "string", "description": "Why it is a problem and how to fix it (Markdown)."},
          "suggestion": {"type": "string", "description": "Optional exact replacement text for lines line..end_line of the new file, without code fences. Empty if not applicable."}
        }
      }
    },
    "resolved": {
      "type": "array",
      "description": "Previously reported open findings that the current code fixes.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["id", "comment"],
        "properties": {
          "id": {"type": "string", "description": "Finding id exactly as given in the prompt."},
          "comment": {"type": "string", "description": "One sentence: how it was fixed."}
        }
      }
    }
  }
}`

// Chat is the JSON Schema of output.ChatOutput passed to Claude.
const Chat = `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "additionalProperties": false,
  "required": ["reply", "resolve"],
  "properties": {
    "reply": {"type": "string", "description": "The answer to post in the discussion (Markdown)."},
    "resolve": {"type": "boolean", "description": "True only if this thread is a bot finding and the discussion concluded it is fixed or not an issue."}
  }
}`
