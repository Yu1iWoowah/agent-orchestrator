import { describe, expect, it } from "vitest";
import { createServer } from "node:net";
import { once } from "node:events";
import { connectAppDiagnostics, privateRuntimeHandoff } from "./app-diagnostics-link";

describe("app diagnostics private link", () => {
 it("hands independent credentials to the daemon without exporting them", () => {
  const [legacy, extra] = privateRuntimeHandoff("browser-token", "app-token").trim().split("\n");
  expect(legacy).toBe("browser-token");
  expect(JSON.parse(extra)).toEqual({ version: 1, browserToken: "browser-token", appDiagnosticsToken: "app-token" });
 });
 it("authenticates separately and never advertises a missing collector", async () => {
  const server = createServer();server.listen(0,"127.0.0.1");await once(server,"listening");const address=server.address();if (!address || typeof address==="string") throw new Error("missing server");
  const frames: unknown[]=[];const accepted=once(server,"connection");const close=connectAppDiagnostics({port:address.port,host:"127.0.0.1"},"app-token","app-instance");
  const [socket]=await accepted;let pending="";
  socket.on("data",(chunk: Buffer)=>{pending+=chunk.toString();for(;;){const end=pending.indexOf("\n");if(end<0)break;frames.push(JSON.parse(pending.slice(0,end)));pending=pending.slice(end+1);}});
  await new Promise<void>(resolve=>socket.once("data",()=>resolve()));
  expect(frames).toEqual([{type:"hello",version:1,token:"app-token",instanceId:"app-instance"}]);
  socket.write(JSON.stringify({type:"hello-ok",version:1,epoch:"epoch-1"})+"\n");
  socket.write(JSON.stringify({type:"snapshot",requestId:"request-1",request:{windowId:"window-1"}})+"\n");
  await new Promise<void>(resolve=>socket.once("data",()=>resolve()));
  expect(frames[1]).toEqual({type:"result",requestId:"request-1",code:"APP_DIAGNOSTICS_UNSUPPORTED"});
  expect(frames.some((frame)=>typeof frame==="object"&&frame!==null&&"type" in frame&&frame.type==="ready")).toBe(false);
  close();socket.destroy();server.close();
 });
});
