const { Web3 } = require('web3');
const web3 = new Web3("http://localhost:8545");

async function run() {
    try {
        const receipt = await web3.eth.getTransactionReceipt("0xaab4900172ba7bc1058fdb2feed33d674dfb82eb893032ae7dbde1930f9242cb");
        console.log("Receipt:", receipt);
    } catch (e) {
        console.error("Error:", e);
    }
}
run();
