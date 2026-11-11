// This closure keeps the host callback and settlement function out of guest globals.
(function (bridge, names, initial, deltaMode) {
  "use strict";
  delete globalThis.__bridge;
  const stringify = JSON.stringify.bind(JSON),
    parse = JSON.parse.bind(JSON);
  const SafeSet = Set,
    SafePromise = Promise,
    SafeError = Error;
  const ownKeys = Reflect.ownKeys.bind(Reflect),
    apply = Reflect.apply.bind(Reflect);
  const getPrototypeOf = Object.getPrototypeOf.bind(Object),
    plainPrototype = Object.prototype,
    hasOwn = Object.prototype.hasOwnProperty;
  const create = Object.create.bind(Object),
    define = Object.defineProperty.bind(Object);
  const isArray = Array.isArray.bind(Array),
    isFinite = Number.isFinite.bind(Number),
    toString = String;
  const setHas = Set.prototype.has,
    setAdd = Set.prototype.add,
    setDelete = Set.prototype.delete;
  const pending = new Map();
  const pendingSet = pending.set.bind(pending),
    pendingGet = pending.get.bind(pending),
    pendingDelete = pending.delete.bind(pending);
  let next = 0;
  function json(value, seen = new SafeSet()) {
    if (
      value === null ||
      typeof value === "string" ||
      typeof value === "boolean"
    )
      return value;
    if (typeof value === "number" && isFinite(value)) return value;
    if (typeof value !== "object" || apply(setHas, seen, [value]))
      throw Error("invalid_json: unsupported or cyclic value");
    apply(setAdd, seen, [value]);
    let result;
    if (isArray(value)) {
      result = [];
      for (let i = 0; i < value.length; i++)
        define(result, toString(i), {
          value: json(value[i], seen),
          enumerable: true,
          configurable: true,
          writable: true,
        });
    } else {
      if (
        getPrototypeOf(value) !== plainPrototype &&
        getPrototypeOf(value) !== null
      )
        throw Error("invalid_json: not a JSON object");
      result = create(null);
      for (const key of ownKeys(value)) {
        if (typeof key !== "string") throw Error("invalid_json: symbol key");
        result[key] = json(value[key], seen);
      }
    }
    apply(setDelete, seen, [value]);
    return result;
  }
  const send = (value) => bridge(stringify(json(value)));
  function request(kind, name, args) {
    const id = ++next;
    return new SafePromise((resolve, reject) => {
      pendingSet(id, { resolve, reject });
      send({ kind, id, name, args });
    });
  }
  const state = Object.assign(create(null), parse(initial));
  const written = create(null);
  const registry = create(null);
  for (const name of names)
    define(registry, name, {
      value: (args) => request("tool", name, args === undefined ? {} : args),
      enumerable: true,
    });
  Object.freeze(registry);
  // Missing names are not registry entries. Only invoking one throws, so
  // discovery, enumeration, and frozen/null-prototype reflection still work.
  const prototypeNames = new SafeSet(ownKeys(plainPrototype));
  const tools = new Proxy(registry, {
    get(target, name) {
      if (typeof name !== "string") return undefined;
      const tool = target[name];
      if (tool !== undefined) return tool;
      // Do not introduce inherited callables or promise assimilation.
      if (name === "then" || apply(setHas, prototypeNames, [name]))
        return undefined;
      return () => {
        const error = SafeError(
          "unknown_tool: " +
            name +
            "; use Object.keys(tools) to list available tools and describeTools to inspect their arguments",
        );
        error.code = "unknown_tool";
        throw error;
      };
    },
  });
  // Observe every input immediately, including when iteration ends early.
  function settleInOrder(promises) {
    const ready = [];
    let wake,
      remaining = promises.length;
    for (let index = 0; index < remaining; index++) {
      const promise = promises[index];
      const push = (result) => {
        ready.push(result);
        if (wake) {
          wake();
          wake = undefined;
        }
      };
      SafePromise.resolve(promise).then(
        (value) => push({ index, status: "fulfilled", value }),
        (reason) => push({ index, status: "rejected", reason }),
      );
    }
    return (async function* () {
      while (remaining > 0) {
        if (ready.length === 0)
          await new SafePromise((resolve) => { wake = resolve; });
        remaining--;
        yield ready.shift();
      }
    })();
  }
  Object.assign(globalThis, {
    tools,
    settleInOrder,
    text: (value) =>
      send({
        kind: "text",
        value: typeof value === "string" ? value : json(value),
      }),
    image: (value) => send({ kind: "image", value }),
    load: (key) => state[toString(key)],
    store: (key, value) => {
      key = toString(key);
      written[key] = true;
      if (value === undefined) delete state[toString(key)];
      else
        define(state, toString(key), {
          value: json(value),
          enumerable: true,
          configurable: true,
          writable: true,
        });
    },
    describeTools: (names) => request("describe", "", names),
    sleep: (ms) => request("sleep", "", ms),
  });
  const settle = function (id, ok, data) {
    const p = pendingGet(id);
    if (!p) return;
    pendingDelete(id);
    const value = parse(data);
    if (ok) p.resolve(value);
    else {
      const e = Error(value.message);
      Object.assign(e, value);
      p.reject(e);
    }
  };
  const diagnostic = function (error) {
    let message = "<unprintable exception>";
    try {
      message = toString(error);
    } catch (_) {}
    try {
      if (
        error !== null &&
        (typeof error === "object" || typeof error === "function")
      ) {
        const stack = error.stack;
        if (typeof stack === "string" && stack !== "") message += "\n" + stack;
      }
    } catch (_) {}
    return message;
  };
  const run = function (body) {
    body()
      .then((value) => {
        if (value !== undefined) globalThis.text(value);
        if (deltaMode) {
          const set = create(null), deleted = [];
          for (const key of ownKeys(written)) {
            if (apply(hasOwn, state, [key])) set[key] = state[key];
            else deleted.push(key);
          }
          send({ kind: "done", value: { set, delete: deleted } });
        } else send({ kind: "done", value: state });
      })
      .catch((e) =>
        send({
          kind: "failed",
          value: diagnostic(e),
        }),
      );
  };
  return { settle, run };
})
