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
