// Prebuilt bundle fixture. Echoes what a pooled worker resolves per
// invocation: the function's own env, the function id, and a dev override.
export const handler = async (event, context) => {
  console.log(`handling ${process.env.HANDLER_FN} request ${context.awsRequestId}`);
  await new Promise((resolve) => setTimeout(resolve, 200));
  return {
    handlerFn: process.env.HANDLER_FN,
    functionId: process.env.SST_FUNCTION_ID,
    functionName: context.functionName,
    resourceEnv: process.env.RESOURCE_ENV,
    pid: process.pid,
    event,
  };
};

// Names the handler for the dev UI, the way a dispatching bundle would.
export const describe = (event) => ({
  key: `round-${event?.round ?? "unknown"}`,
  label: `round ${event?.round ?? "unknown"} #${event?.n ?? "?"}`,
});
