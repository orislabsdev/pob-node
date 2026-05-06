const { Web3 } = require('web3');
const web3 = new Web3("http://localhost:8545");

async function run() {
    try {
        const tx = await web3.eth.getTransaction("0xaab4900172ba7bc1058fdb2feed33d674dfb82eb893032ae7dbde1930f9242cb");
        console.log("Transaction To:", tx.to);
        console.log("Transaction Data length:", tx.input.length);
    } catch (e) {
        console.error("Error:", e);
    }
}
run();
