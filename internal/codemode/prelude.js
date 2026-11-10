// This closure keeps the host callback and settlement function out of guest globals.
(function (bridge, names, initial) {
  "use strict";
  delete globalThis.__bridge;
  const stringify = JSON.stringify.bind(JSON),
    parse = JSON.parse.bind(JSON);
  const SafeSet = Set,
    SafePromise = Promise;
  const ownKeys = Reflect.ownKeys.bind(Reflect),
    apply = Reflect.apply.bind(Reflect);
  const getPrototypeOf = Object.getPrototypeOf.bind(Object),
    plainPrototype = Object.prototype;
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
  const tools = create(null);
  for (const name of names)
    Object.defineProperty(tools, name, {
      value: (args) => request("tool", name, args === undefined ? {} : args),
      enumerable: true,
    });
  Object.freeze(tools);
  Object.assign(globalThis, {
    tools,
    text: (value) =>
      send({
        kind: "text",
        value: typeof value === "string" ? value : json(value),
      }),
    image: (value) => send({ kind: "image", value }),
    load: (key) => state[toString(key)],
    store: (key, value) => {
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
  const run = function (body) {
    body()
      .then((value) => {
        if (value !== undefined) globalThis.text(value);
        send({ kind: "done", value: state });
      })
      .catch((e) =>
        send({
          kind: "failed",
          value: String(e) + "\n" + String((e && e.stack) || ""),
        }),
      );
  };
  return { settle, run };
})
