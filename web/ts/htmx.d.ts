// The slice of htmx's API (v2, loaded from /static/htmx.min.js) and of its
// events that app.ts uses. Declared here so type-checking needs no
// node_modules.
interface HtmxConfirmDetail {
  elt: HTMLElement;
  question: string | null;
  issueRequest(skipConfirmation?: boolean): void;
}

interface HtmxResponseDetail {
  xhr: XMLHttpRequest;
  elt: HTMLElement;
  successful: boolean;
  boosted?: boolean;
}

interface ToastDetail {
  kind: "ok" | "err";
  msg: string;
}

interface DocumentEventMap {
  "htmx:confirm": CustomEvent<HtmxConfirmDetail>;
  "htmx:load": CustomEvent<{ elt: HTMLElement }>;
  "htmx:afterSwap": CustomEvent<HtmxResponseDetail>;
  "htmx:beforeSwap": CustomEvent<HtmxResponseDetail>;
  "htmx:beforeRequest": CustomEvent<HtmxResponseDetail>;
  "htmx:afterRequest": CustomEvent<HtmxResponseDetail>;
  "htmx:sendError": CustomEvent<HtmxResponseDetail>;
  "htmx:responseError": CustomEvent<HtmxResponseDetail>;
  toast: CustomEvent<ToastDetail>;
  "link-created": CustomEvent<{ url: string }>;
}

declare const htmx: {
  ajax(verb: string, path: string, context?: object): Promise<void>;
  trigger(elt: Element | string, name: string, detail?: object): void;
};
