/** One problem in a scene document, addressed by a path like `bodies[1].shape.radius`. */
export interface SceneIssue {
  /** Empty only for errors about the document as a whole. */
  path: string;
  message: string;
}

/**
 * A scene document that cannot become a world. `kind` says which gate rejected it:
 * `syntax` = wrong shape of data (zod), `semantics` = well-formed but physically invalid.
 * Every issue is reported at once so a generator (human or LLM) can fix them in one pass.
 */
export class SceneError extends Error {
  constructor(
    readonly kind: 'syntax' | 'semantics',
    readonly issues: SceneIssue[],
  ) {
    const label = kind === 'syntax' ? 'invalid scene JSON' : 'invalid scene';
    super([label, ...issues.map((issue) => (issue.path ? `${issue.path}: ${issue.message}` : issue.message))].join('\n'));
    this.name = 'SceneError';
  }
}
